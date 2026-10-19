package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	task "go-lua-crawler/internal/task"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

type childParameters struct {
	Params   map[string]any `json:"params"`
	Spec     TaskSpec       `json:"spec"`
	SourceID string         `json:"source_id"`
}
type AcquisitionResult struct {
	Records                []Record             `json:"records"`
	Coverage               CoverageReport       `json:"coverage"`
	Runtime                engine.ProgramResult `json:"runtime"`
	SemanticSupportChecked bool                 `json:"semantic_support_checked"`
}

// ExecuteTask wraps the existing worker entry point. Generated code goes only to
// the private child-process runner; trusted legacy rules keep their old entry.
func (s *Service) ExecuteTask(ctx context.Context, child task.CrawlTask) (repository.PersistTaskResult, error) {
	if child.ProgramHash == "" {
		return engine.ExecuteTask(ctx, child)
	}
	if s.Runner == nil {
		return nil, fmt.Errorf("private Lua runner not configured")
	}
	r, err := s.Data.Run(ctx, child.RunID)
	if err != nil {
		return nil, err
	}
	if r.Cancelled || r.IsTerminal() || r.Expired(time.Now()) {
		return nil, agentdata.ErrConflict
	}
	a, err := s.Data.Artifact(ctx, child.ProgramHash)
	if err != nil {
		return nil, err
	}
	var args childParameters
	if err = json.Unmarshal([]byte(child.ProgramParams), &args); err != nil {
		return nil, err
	}
	if err = ValidateParameters(a.InputSchema, args.Params); err != nil {
		return nil, err
	}
	var domains []string
	if err = json.Unmarshal([]byte(a.ScopeJSON), &domains); err != nil {
		return nil, err
	}
	if args.SourceID != "" {
		if err := requireActiveSourceVersion(s.Data.DB.WithContext(ctx), args.SourceID, a.Hash, false); err != nil {
			return nil, err
		}
	}
	ctx, cancel := deliveryRequestContext(ctx, r)
	defer cancel()
	limits := programLimitsForRun(r)
	// Reserve every physical worker attempt, including an adopted expired lease.
	// Unlike parent action selection, this deterministic ledger does not need a Run lease.
	var attempt agentdata.Operation
	err = s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current agentdata.Run
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", r.ID).Error; err != nil {
			return err
		}
		if current.Cancelled || current.IsTerminal() || current.Expired(time.Now()) {
			return agentdata.ErrConflict
		}
		var snapshot BudgetSnapshot
		if err := json.Unmarshal([]byte(current.BudgetJSON), &snapshot); err != nil {
			return err
		}
		b, err := NewRunBudget(snapshot)
		if err != nil {
			return err
		}
		if err = b.Reserve(CallCost{HTTPRequests: int64(limits.MaxRequests), HTTPBytes: limits.MaxBytes}, time.Now()); err != nil {
			return err
		}
		raw, _ := json.Marshal(b.Snapshot())
		if err = tx.Model(&agentdata.Run{}).Where("id = ?", r.ID).Update("budget_json", string(raw)).Error; err != nil {
			return err
		}
		resources, _ := json.Marshal(CallCost{HTTPRequests: int64(limits.MaxRequests), HTTPBytes: limits.MaxBytes})
		attempt = agentdata.Operation{RunID: r.ID, Key: "worker:" + agentdata.Token(), InputHash: agentdata.Hash([]any{child.ID, a.Hash}), Kind: "worker_attempt", Stage: "reserved", ResourcesJSON: string(resources)}
		return tx.Create(&attempt).Error
	})
	if err != nil {
		return nil, err
	}
	result, err := s.Runner.Execute(ctx, RunnerRequest{Program: engine.ProgramRequest{Code: a.Code, TargetURL: child.URL, Params: args.Params}, Limits: limits, AllowedDomains: domains})
	stats, _ := json.Marshal(map[string]any{"task_id": child.ID, "runtime": result, "outcome_known": result.HTTPUsageKnown, "error": fmt.Sprint(err)})
	stage := "observed"
	if !result.HTTPUsageKnown {
		stage = "unknown_external"
	}
	if saveErr := s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current agentdata.Run
		if e := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", r.ID).Error; e != nil {
			return e
		}
		var saved agentdata.Operation
		if e := tx.First(&saved, "id = ? AND run_id = ? AND stage = ?", attempt.ID, r.ID, "reserved").Error; e != nil {
			return e
		}
		next, e := settleHTTPBudget(current.BudgetJSON, saved, &HTTPObservation{Known: result.HTTPUsageKnown, Requests: result.Stats.Requests, Bytes: result.Stats.Bytes})
		if e != nil {
			return e
		}
		if e = tx.Model(&agentdata.Run{}).Where("id = ?", r.ID).Update("budget_json", next).Error; e != nil {
			return e
		}
		return tx.Model(&agentdata.Operation{}).Where("id = ? AND stage = ?", attempt.ID, "reserved").Updates(map[string]any{"stage": stage, "result_json": string(stats)}).Error
	}); saveErr != nil {
		return nil, saveErr
	}
	if err != nil {
		return nil, err
	}
	source := args.SourceID
	if source == "" {
		source = "composition"
	}
	records, err := BindRecords(result, RecordBinding{SourceID: source, DataVersion: a.Hash, TaskID: child.ID, SchemaVersion: args.Spec.SchemaVersion, KeyFields: args.Spec.JoinKeys})
	if err != nil {
		return nil, err
	}
	var output TaskSpec
	if err = requireSupportedFields(args.Spec, records); err != nil {
		return nil, err
	}
	if err = json.Unmarshal([]byte(a.OutputSchema), &output); err != nil {
		return nil, err
	}
	// Only a concrete field-contract violation invalidates a registered version.
	// Missing entities, stale data and network failures are task gaps, not proof
	// that this reusable adapter is broken. Never quarantine a newer source head.
	for _, record := range records {
		for _, field := range output.RequiredFields {
			if !fieldMatches(record.Fields[field.Name], field.Type) {
				violation := fmt.Errorf("acquired output violates field contract: %s (%s)", field.Name, field.Type)
				if args.SourceID != "" {
					if err := s.QuarantineSource(ctx, args.SourceID, a.Hash, violation.Error()); err != nil && !errors.Is(err, agentdata.ErrConflict) {
						return nil, fmt.Errorf("%v; quarantine failed: %w", violation, err)
					}
				}
				return nil, violation
			}
		}
	}
	coverage, err := EvaluateRecords(output, records, time.Now())
	if err != nil {
		return nil, err
	}
	if len(coverage.Issues) > 0 {
		return nil, fmt.Errorf("acquired output violates frozen schema/provenance: %v", coverage.Issues)
	}
	return s.persistAcquisition(child, AcquisitionResult{Records: records, Coverage: coverage, Runtime: result}), nil
}
func (s *Service) persistAcquisition(child task.CrawlTask, result AcquisitionResult) repository.PersistTaskResult {
	return func(tx *gorm.DB) error {
		var bound childParameters
		if child.ProgramParams != "" {
			if err := json.Unmarshal([]byte(child.ProgramParams), &bound); err != nil {
				return err
			}
		}
		if err := requireSupportedFields(bound.Spec, result.Records); err != nil {
			return err
		}
		var r agentdata.Run
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, "id = ?", child.RunID).Error; err != nil {
			return err
		}
		if r.Cancelled || r.IsTerminal() || r.Expired(time.Now()) {
			return agentdata.ErrConflict
		}
		var args childParameters
		if child.ProgramParams != "" {
			if err := json.Unmarshal([]byte(child.ProgramParams), &args); err != nil {
				return err
			}
		}
		if args.SourceID != "" {
			if err := requireActiveSourceVersion(tx, args.SourceID, child.ProgramHash, true); err != nil {
				return err
			}
		}
		payload, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if err = tx.Create(&repository.CrawlResult{TaskID: child.ID, Payload: string(payload)}).Error; err != nil {
			return err
		}
		documents, err := AcquisitionDocuments(result.Records, args.Spec)
		if err != nil {
			return err
		}
		for _, doc := range documents {
			if _, err := knowledge.PutDocument(tx, doc, s.Fingerprint); err != nil {
				return err
			}
		}
		return tx.Model(&agentdata.Run{}).Where("id = ? AND status = ?", child.RunID, "waiting").Update("next_wake_at", time.Now()).Error
	}
}

// The source head selects versions for NEW work. In-flight work owns its
// immutable published version until that version or the source is revoked.
func requireActiveSourceVersion(db *gorm.DB, source, hash string, lock bool) error {
	headQuery, versionQuery := db.Session(&gorm.Session{}), db.Session(&gorm.Session{})
	if lock {
		headQuery = headQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		versionQuery = versionQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var head agentdata.SourceHead
	if err := headQuery.First(&head, "source_id = ?", source).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("%w: source_id %q is not registered", agentdata.ErrConflict, source)
		}
		return err
	}
	var version agentdata.SourceVersion
	if err := versionQuery.Where("source_id = ? AND artifact_hash = ?", source, hash).First(&version).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return fmt.Errorf("%w: source %q has no published version %q", agentdata.ErrConflict, source, hash)
		}
		return err
	}
	if head.Status != "active" || version.Status != "active" {
		return fmt.Errorf("%w: source %q status=%q, bound version %q status=%q", agentdata.ErrConflict, source, head.Status, hash, version.Status)
	}
	return nil
}

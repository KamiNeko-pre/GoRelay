package information

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/agentstatus"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	task "go-lua-crawler/internal/task"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type AssetTrial struct {
	ID          string    `json:"id" gorm:"primaryKey;size:64"`
	RequestID   string    `json:"request_id" gorm:"size:32;index"`
	AssetJSON   string    `json:"-" gorm:"type:longtext"`
	RuntimeJSON string    `json:"-" gorm:"type:longtext"`
	CreatedAt   time.Time `json:"created_at"`
}

func (AssetTrial) TableName() string { return "information_asset_trials" }

type TrialResult struct {
	CandidateID string               `json:"candidate_id"`
	Asset       Asset                `json:"asset"`
	Coverage    aiapp.CoverageReport `json:"coverage"`
	Records     []aiapp.Record       `json:"records"`
}

type ActionError struct {
	Code        string         `json:"code"`
	Message     string         `json:"message"`
	Steps       []string       `json:"steps"`
	PriorStepID uint64         `json:"prior_step_id,omitempty"`
	Cause       error          `json:"-"`
	Details     map[string]any `json:"details,omitempty"`
}

func (e *ActionError) Error() string { return e.Code + ": " + e.Message }
func (e *ActionError) Unwrap() error { return e.Cause }
func (e *ActionError) OperationalStatus() agentstatus.Status {
	s := aiapp.OperationalStatus(e.Cause)
	switch e.Code {
	case "ACTION_ALREADY_CALLED", "ACTION_OUTCOME_UNKNOWN":
		s = agentstatus.Of(agentstatus.AgentDuplicate)
	case "ROUTING_ARGUMENTS_INVALID", "ASSET_SCOPE_INVALID", "LOCAL_LOOKUP_REQUIRED", "LUA_FAILURE_REPORT_INVALID", "BROWSER_SOURCE_REFERENCE_INVALID":
		s = agentstatus.Of(agentstatus.AgentCorrection)
	case "BROWSER_UNAVAILABLE", "ROUTING_MODEL_UNAVAILABLE":
		s = agentstatus.Of(agentstatus.SystemConfiguration)
	case "ROUTING_OUTPUT_INCOMPLETE":
		s = agentstatus.Of(agentstatus.SystemDependency)
	case "ASSET_TRIAL_FAILED", "ASSET_EXECUTION_FAILED":
		if s.Code == agentstatus.Unclassified {
			s = agentstatus.Of(agentstatus.AgentCorrection)
		}
	case "LUA_FAILURE_RECORDED":
		s = agentstatus.Of(agentstatus.AgentExhausted)
	}
	s.Recovery = append([]string{}, e.Steps...)
	return s
}
func actionFailure(code string, cause error, steps ...string) *ActionError {
	return &ActionError{Code: code, Message: cause.Error(), Steps: steps, Cause: cause}
}

func (s *Service) trialRequest(ctx context.Context, id string) (Request, Input, aiapp.TaskSpec, error) {
	r, err := s.Request(ctx, id)
	if err != nil {
		return r, Input{}, aiapp.TaskSpec{}, err
	}
	var in Input
	json.Unmarshal([]byte(r.InputJSON), &in)
	if !r.Deadline.IsZero() && time.Now().After(r.Deadline) {
		return r, in, aiapp.TaskSpec{}, fmt.Errorf("information request deadline reached")
	}
	var count int64
	if err := s.DB.WithContext(ctx).Model(&Step{}).Where("request_id = ? AND kind = ? AND stage = ?", id, "local_lookup", "succeeded").Count(&count).Error; err != nil {
		return r, in, aiapp.TaskSpec{}, err
	}
	if count == 0 {
		return r, in, aiapp.TaskSpec{}, actionFailure("LOCAL_LOOKUP_REQUIRED", fmt.Errorf("local assets have not been checked"), "Call the host Preflight before external acquisition.")
	}
	var spec aiapp.TaskSpec
	if r.SpecJSON == "" {
		return r, in, spec, fmt.Errorf("freeze requested output fields before validating an acquisition asset")
	}
	if err := json.Unmarshal([]byte(r.SpecJSON), &spec); err != nil {
		return r, in, spec, err
	}
	in.Spec = &spec
	return r, in, spec, nil
}

func duplicateStep(step Step) *ActionError {
	code := "ACTION_ALREADY_CALLED"
	if step.Stage == "reserved" {
		code = "ACTION_OUTCOME_UNKNOWN"
	}
	return &ActionError{Code: code, Message: "same action and effective business parameters were already admitted; changing call metadata cannot repeat execution", PriorStepID: step.ID, Steps: []string{"Read the saved step result and its exact failure cause.", "Change a real URL, field pointer or acquisition plan only when supported by new source evidence; otherwise use a different applicable asset."}}
}

func (s *Service) TrialAPI(ctx context.Context, id, description string, plan APIPlan) (TrialResult, error) {
	r, in, spec, err := s.trialRequest(ctx, id)
	if err != nil {
		return TrialResult{}, err
	}
	if !urlInScope(plan.URL, in.SourceScope) {
		return TrialResult{}, actionFailure("ASSET_SCOPE_INVALID", fmt.Errorf("API URL is outside the original scope or malformed"), "Choose a public HTTP(S) endpoint inside the granted source scope.")
	}
	input := map[string]any{"api": plan, "scope": in.SourceScope, "spec": spec}
	step, fresh, err := s.reserveStep(ctx, r, "trial_api", input)
	if err != nil {
		return TrialResult{}, err
	}
	if !fresh {
		return TrialResult{}, duplicateStep(step)
	}
	ctx, cancel := r.RequestContext(ctx)
	defer cancel()
	fetcher, err := s.NewFetcher(in.SourceScope)
	if err != nil {
		s.finishStep(context.WithoutCancel(ctx), step, nil, err)
		return TrialResult{}, err
	}
	defer fetcher.Close()
	runtime, cause := executeAPI(ctx, plan, fetcher.Fetch)
	runtime.Stats = fetcher.Stats()
	asset := Asset{Kind: "api", SourceID: "api-" + agentdata.Hash(plan)[:24], Version: agentdata.Hash(input), Description: description, EntryURL: plan.URL, SpecJSON: r.SpecJSON, ScopeJSON: string(mustMarshal(in.SourceScope)), ManifestJSON: string(mustMarshal(plan)), VerifiedAt: time.Now()}
	asset.ID = agentdata.Hash([]string{asset.Kind, asset.SourceID, asset.Version})
	out, err := s.saveTrial(ctx, r, step, asset, runtime, cause)
	return out, err
}

func (s *Service) saveTrial(ctx context.Context, r Request, step Step, a Asset, runtime engine.ProgramResult, cause error) (TrialResult, error) {
	out := TrialResult{Asset: a}
	var spec aiapp.TaskSpec
	json.Unmarshal([]byte(r.SpecJSON), &spec)
	if cause == nil {
		out.Records, cause = aiapp.BindRecords(runtime, aiapp.RecordBinding{SourceID: a.SourceID, DataVersion: a.Version, TaskID: step.ID, SchemaVersion: spec.SchemaVersion, KeyFields: spec.JoinKeys})
	}
	if cause == nil {
		out.Coverage, cause = aiapp.EvaluateRecords(spec, out.Records, time.Now())
		if cause == nil && !out.Coverage.Satisfied {
			cause = fmt.Errorf("acquisition does not satisfy frozen output: %+v", out.Coverage)
		}
	}
	if cause != nil {
		failure := actionFailure("ASSET_TRIAL_FAILED", cause, "Inspect the saved successful/failed HTTP receipts and missing JSON paths or fields.", "Correct the plan from actual source structure; never invent a value or repeat an unchanged plan.", "If the source cannot supply the frozen fields, continue to another verified API or Lua/Browser acquisition.")
		failure.PriorStepID = step.ID
		failure.Details = map[string]any{"coverage": out.Coverage}
		if a.Kind == "api" {
			failure.Details["execution_contract"] = nativeAPIExecutionContract
			failure.Steps = []string{"Read diagnostic.details.coverage and prior_step.result.runtime for actual missing entities, fields and HTTP receipts.", "Each native API trial validates the whole frozen contract; partial results from separate trials are not accumulated. Correct JSON pointers only from observed structure.", "If independent endpoints, fan-out or transformations are required, choose api_unsuitable with that capability reason so the Lua program can perform the complete acquisition. Otherwise trial a verified endpoint whose pagination covers the whole contract."}
		}
		if err := s.finishStep(context.WithoutCancel(ctx), step, map[string]any{"failure": failure, "coverage": out.Coverage, "runtime": runtime}, failure); err != nil {
			return out, err
		}
		return out, failure
	}
	out.CandidateID = agentdata.Hash([]any{r.ID, a.ID, step.InputHash})
	proof := map[string]any{"candidate_id": out.CandidateID, "step_id": step.ID, "coverage": out.Coverage, "runtime_hash": agentdata.Hash(runtime)}
	if a.Kind == "browser" {
		proof["browser_approval_request"] = r.ID
	}
	a.ProofJSON = string(mustMarshal(proof))
	out.Asset = a
	trial := AssetTrial{ID: out.CandidateID, RequestID: r.ID, AssetJSON: string(mustMarshal(a)), RuntimeJSON: string(mustMarshal(runtime))}
	if err := s.DB.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&trial).Error; err != nil {
		return out, err
	}
	if err := s.finishStep(context.WithoutCancel(ctx), step, out, nil); err != nil {
		return out, err
	}
	return out, nil
}

func (s *Service) Publish(ctx context.Context, requestID, candidateID string) (Asset, error) {
	var trial AssetTrial
	if err := s.DB.WithContext(ctx).First(&trial, "id = ? AND request_id = ?", candidateID, requestID).Error; err != nil {
		return Asset{}, actionFailure("ASSET_CANDIDATE_INVALID", err, "Copy candidate_id from a successful acquisition trial in this request.")
	}
	var a Asset
	if err := json.Unmarshal([]byte(trial.AssetJSON), &a); err != nil {
		return a, err
	}
	a.Active = true
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&a).Error; err != nil {
			return err
		}
		return s.indexAsset(tx, a, requestID)
	})
	return a, err
}

func (s *Service) indexAsset(tx *gorm.DB, a Asset, requestIDs ...string) error {
	body := fmt.Sprintf("Acquisition asset %s\nKind: %s\n%s\nOutput contract: %s\nScope: %s", a.SourceID, a.Kind, a.Description, a.SpecJSON, a.ScopeJSON)
	doc, err := knowledge.NewDocument(knowledge.Input{Source: "information-capability", SourceKey: a.ID, Title: a.SourceID, URL: a.EntryURL, Body: body, Format: "text", FetchedAt: a.VerifiedAt, DataVersion: a.Version, RecordKey: a.ID})
	if err != nil {
		return err
	}
	_, err = knowledge.PutDocument(tx, doc, s.Crawler.Fingerprint, requestIDs...)
	return err
}

// A native asset's actual execution uses the same immutable declarative plan
// that passed trial. It commits standard typed acquisition snapshots, allowing
// the existing knowledge indexer/retriever to read them without crawler tools.
func (s *Service) ExecuteAPI(ctx context.Context, id, assetID string) ([]aiapp.Record, error) {
	var a Asset
	if err := s.DB.WithContext(ctx).First(&a, "id = ? AND kind = ?", assetID, "api").Error; err != nil {
		return nil, err
	}
	return s.ExecuteAsset(ctx, id, assetID)
}

func (s *Service) ExecuteAsset(ctx context.Context, id, assetID string) ([]aiapp.Record, error) {
	r, in, spec, err := s.trialRequest(ctx, id)
	if err != nil {
		return nil, err
	}
	var a Asset
	if err := s.DB.WithContext(ctx).First(&a, "id = ? AND active = ?", assetID, true).Error; err != nil {
		return nil, err
	}
	if !assetFits(a, in) {
		return nil, fmt.Errorf("asset is not compatible with the frozen field/scope contract")
	}
	if err := s.requireEligibleAsset(ctx, r, a); err != nil {
		return nil, err
	}
	if a.Kind != "api" && a.Kind != "browser" {
		return nil, fmt.Errorf("Lua assets execute only through the independent crawler entry")
	}
	input := map[string]any{"asset": a.ID, "version": a.Version, "manifest": a.ManifestJSON, "spec": spec}
	var step Step
	var fresh bool
	if a.Kind == "browser" {
		if err := s.grantBrowserAsset(ctx, r, a); err != nil {
			return nil, err
		}
		step, fresh, err = s.reserveBrowserStep(ctx, r, "execute_browser", a.EntryURL, input)
	} else {
		step, fresh, err = s.reserveStep(ctx, r, "execute_asset", input)
	}
	if err != nil {
		return nil, err
	}
	if !fresh {
		if step.Stage == "reserved" {
			var restored []aiapp.Record
			err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				if err := requireNativeCommitOwner(tx, ctx, r, step); err != nil {
					return err
				}
				var err error
				restored, _, err = committedNativeRows(tx, r, step, a)
				return err
			})
			if err != nil {
				return nil, err
			}
			if restored != nil {
				if err := s.finishStep(ctx, step, map[string]any{"records": restored, "restored": true}, nil); err != nil {
					return nil, err
				}
				return restored, nil
			}
		}
		return nil, duplicateStep(step)
	}
	ctx, cancel := r.RequestContext(ctx)
	defer cancel()
	fetcher, err := s.NewFetcher(in.SourceScope)
	if err != nil {
		s.finishStep(context.WithoutCancel(ctx), step, nil, err)
		return nil, err
	}
	defer fetcher.Close()
	var runtime engine.ProgramResult
	var cause error
	if a.Kind == "api" {
		var plan APIPlan
		cause = json.Unmarshal([]byte(a.ManifestJSON), &plan)
		if cause == nil {
			runtime, cause = executeAPI(ctx, plan, fetcher.Fetch)
		}
	} else {
		var plan BrowserPlan
		cause = json.Unmarshal([]byte(a.ManifestJSON), &plan)
		if cause == nil && s.Browser == nil {
			cause = fmt.Errorf("browser runtime is not configured; configure its executable, Node and Playwright module")
		}
		if cause == nil {
			runtime, cause = s.Browser.Render(ctx, plan, in.SourceScope, fetcher)
		}
	}
	runtime.Stats = fetcher.Stats()
	var rows []aiapp.Record
	if cause == nil {
		rows, cause = s.commitNative(ctx, r, step, a, spec, runtime)
	}
	if cause != nil {
		cause = actionFailure("ASSET_EXECUTION_FAILED", cause, "Inspect the saved HTTP result and schema mismatch.", "Keep the published version immutable; trial a corrected plan as a new version, or select another acquisition path.")
	}
	if err := s.finishStep(context.WithoutCancel(ctx), step, map[string]any{"records": rows, "runtime": runtime}, cause); err != nil {
		return nil, err
	}
	return rows, cause
}

func (s *Service) commitNative(ctx context.Context, r Request, step Step, a Asset, spec aiapp.TaskSpec, runtime engine.ProgramResult) ([]aiapp.Record, error) {
	var rows []aiapp.Record
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := requireNativeCommitOwner(tx, ctx, r, step); err != nil {
			return err
		}
		var current Asset
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ? AND active = ?", a.ID, true).Error; err != nil {
			return err
		}
		if current.Version != a.Version {
			return agentdata.ErrConflict
		}
		if saved, found, err := committedNativeRows(tx, r, step, a); err != nil {
			return err
		} else if found {
			rows = saved
			return nil
		}
		at := time.Now()
		child := task.CrawlTask{RequestKey: "information-" + r.ID + "-" + fmt.Sprint(step.ID), Target: "information-" + a.Kind, URL: a.EntryURL, Status: task.StatusSucceeded, StartedAt: &at, FinishedAt: &at, ProgramHash: a.Version}
		// Terminal facts only: no Outbox/Stream is created and no crawler worker
		// is asked to execute this API/browser acquisition.
		if err := tx.Create(&child).Error; err != nil {
			return err
		}
		var err error
		rows, err = aiapp.BindRecords(runtime, aiapp.RecordBinding{SourceID: a.SourceID, DataVersion: a.Version, TaskID: child.ID, SchemaVersion: spec.SchemaVersion, KeyFields: spec.JoinKeys})
		if err != nil {
			return err
		}
		coverage, err := aiapp.EvaluateRecords(spec, rows, time.Now())
		if err != nil {
			return err
		}
		if !coverage.Satisfied {
			return fmt.Errorf("asset no longer meets the frozen output contract: %+v", coverage)
		}
		if err := tx.Create(&repository.CrawlResult{TaskID: child.ID, Payload: string(mustMarshal(aiapp.AcquisitionResult{Records: rows, Coverage: coverage, Runtime: runtime}))}).Error; err != nil {
			return err
		}
		documents, err := aiapp.AcquisitionDocuments(rows, spec)
		if err != nil {
			return err
		}
		for _, doc := range documents {
			if _, err := knowledge.PutDocument(tx, doc, s.Crawler.Fingerprint, r.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return rows, err
}

func requireNativeCommitOwner(tx *gorm.DB, ctx context.Context, r Request, step Step) error {
	if step.RequestID != r.ID {
		return agentdata.ErrConflict
	}
	var current Request
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", r.ID).Error; err != nil {
		return err
	}
	expected := r.LeaseToken
	if owner, ok := ctx.Value(requestExecutionOwnerKey{}).(requestExecutionOwner); ok {
		if owner.RequestID != r.ID {
			return agentdata.ErrConflict
		}
		expected = owner.Token
	}
	now := time.Now()
	if terminal(current.Stage) || !current.Deadline.IsZero() && !current.Deadline.After(now) || current.LeaseToken != expected {
		return agentdata.ErrConflict
	}
	if expected != "" && (current.LeaseUntil == nil || !current.LeaseUntil.After(now)) {
		return agentdata.ErrConflict
	}
	return nil
}

func committedNativeRows(tx *gorm.DB, r Request, step Step, a Asset) ([]aiapp.Record, bool, error) {
	var saved task.CrawlTask
	err := tx.Where("request_key = ?", "information-"+r.ID+"-"+fmt.Sprint(step.ID)).First(&saved).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if saved.Status != task.StatusSucceeded || saved.ProgramHash != a.Version {
		return nil, false, agentdata.ErrConflict
	}
	var receipt repository.CrawlResult
	if err := tx.First(&receipt, "task_id = ?", saved.ID).Error; err != nil {
		return nil, false, err
	}
	var result aiapp.AcquisitionResult
	if err := json.Unmarshal([]byte(receipt.Payload), &result); err != nil {
		return nil, false, err
	}
	return result.Records, true, nil
}

func (s *Service) Revoke(ctx context.Context, id string) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var a Asset
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&a, "id = ?", id).Error; err != nil {
			return err
		}
		if err := tx.Model(&a).Update("active", false).Error; err != nil {
			return err
		}
		var heads []knowledge.Head
		if err := tx.Where("(source = ? AND record_key = ?) OR (source = ? AND data_version = ?)", "information-capability", a.ID, a.SourceID, a.Version).Find(&heads).Error; err != nil {
			return err
		}
		for _, h := range heads {
			if err := knowledge.NewStore(tx).Invalidate(ctx, h.ID); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		return nil
	})
}

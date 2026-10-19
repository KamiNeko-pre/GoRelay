package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"gorm.io/gorm"
	"strings"
	"time"
)

type RecordReplacement struct {
	ID           uint64    `json:"id" gorm:"primaryKey"`
	RunID        string    `json:"run_id" gorm:"size:32;index"`
	OldTaskID    uint64    `json:"old_task_id"`
	OldRecordKey string    `json:"old_record_key" gorm:"type:text"`
	NewTaskID    uint64    `json:"new_task_id"`
	NewRecordKey string    `json:"new_record_key" gorm:"type:text"`
	Reason       string    `json:"reason" gorm:"type:text"`
	CreatedAt    time.Time `json:"created_at"`
}

func recordIdentity(task uint64, key string) string { return agentdata.Hash([]any{task, key}) }
func (s *Service) replacements(ctx context.Context, id string) ([]RecordReplacement, error) {
	rows := []RecordReplacement{}
	err := s.Data.DB.WithContext(ctx).Where("run_id = ?", id).Order("id").Find(&rows).Error
	return rows, err
}

func (s *Service) replaceRecord(ctx context.Context, r agentdata.Run, call ToolCall) (ToolResult, error) {
	var out ToolResult
	err := s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		copy := *s
		copy.Data = agentdata.NewStore(tx)
		copy.Knowledge = knowledge.NewStore(tx)
		var err error
		out, err = copy.persistReplacement(ctx, r, call)
		return err
	})
	return out, err
}
func (s *Service) persistReplacement(ctx context.Context, r agentdata.Run, call ToolCall) (ToolResult, error) {
	var args RecordReplacement
	if err := decodeArgs(call.Function.Arguments, &args); err != nil {
		return ToolResult{}, err
	}
	if args.NewTaskID <= args.OldTaskID || strings.TrimSpace(args.Reason) == "" || len(args.Reason) > 2000 {
		return ToolResult{}, fmt.Errorf("newer replacement task and concrete reason required")
	}
	spec, err := specOf(r)
	if err != nil {
		return ToolResult{}, err
	}
	if len(spec.JoinKeys) == 0 {
		return ToolResult{}, fmt.Errorf("explicit business join keys required for replacement")
	}
	rows, err := s.ChildResults(ctx, r.ID)
	if err != nil {
		return ToolResult{}, err
	}
	var old, fresh *Record
	for _, row := range rows {
		for _, record := range row.Records {
			copy := record
			if row.Task.ID == args.OldTaskID && record.Key == args.OldRecordKey {
				old = &copy
			}
			if row.Task.ID == args.NewTaskID && record.Key == args.NewRecordKey {
				fresh = &copy
			}
		}
	}
	if old == nil || fresh == nil {
		return ToolResult{}, fmt.Errorf("both records must belong to successful children of this Run")
	}
	if old.SchemaVersion != spec.SchemaVersion || fresh.SchemaVersion != spec.SchemaVersion || fresh.Provenance.FetchedAt.Before(old.Provenance.FetchedAt) || !recordInScope(*fresh, scope(r)) {
		return ToolResult{}, fmt.Errorf("replacement schema, freshness or scope is ineligible")
	}
	for _, key := range spec.JoinKeys {
		a, _ := json.Marshal(old.Fields[key])
		b, _ := json.Marshal(fresh.Fields[key])
		if old.Fields[key] == nil || string(a) != string(b) {
			return ToolResult{}, fmt.Errorf("replacement changes business identity")
		}
	}
	// The replacement must preserve every previously useful required field. It
	// can improve a partial record, but cannot erase complementary deliverables.
	needed := []FieldRequirement{}
	for _, field := range spec.RequiredFields {
		if fieldMatches(old.Fields[field.Name], field.Type) {
			needed = append(needed, field)
		}
	}
	check := spec
	check.RequiredFields = needed
	check.Entities = nil
	check.EntityField = ""
	coverage, err := EvaluateRecords(check, []Record{*fresh}, time.Now())
	if err != nil || coverage.ValidRecords != 1 {
		return ToolResult{}, fmt.Errorf("replacement loses required fields or current provenance")
	}
	current, err := s.effectiveChildren(ctx, r.ID, rows)
	if err != nil {
		return ToolResult{}, err
	}
	activeOld, activeNew := false, false
	for _, row := range current {
		for _, record := range row.Records {
			if row.Task.ID == args.OldTaskID && record.Key == args.OldRecordKey {
				activeOld = true
			}
			if row.Task.ID == args.NewTaskID && record.Key == args.NewRecordKey {
				activeNew = true
			}
		}
	}
	if !activeOld || !activeNew {
		return ToolResult{}, fmt.Errorf("replacement endpoints must still be effective")
	}
	args.ID = 0
	args.RunID = r.ID
	args.CreatedAt = time.Now()
	if err = s.Data.DB.WithContext(ctx).Create(&args).Error; err != nil {
		return ToolResult{}, err
	}
	// Exclude old projected evidence before the tool receipt commits. History and
	// cached vectors remain; other records in the same task stay current.
	var heads []knowledge.Head
	if err = s.Data.DB.WithContext(ctx).Where("task_id = ? AND record_key = ? AND active = ?", args.OldTaskID, args.OldRecordKey, true).Find(&heads).Error; err != nil {
		return ToolResult{}, err
	}
	for _, head := range heads {
		if err = s.Knowledge.Invalidate(ctx, head.ID); err != nil {
			return ToolResult{}, err
		}
	}
	return toolData(call, args)
}

func (s *Service) effectiveChildren(ctx context.Context, id string, rows []ChildResult) ([]ChildResult, error) {
	revisions, err := s.replacements(ctx, id)
	if err != nil {
		return nil, err
	}
	excluded := map[string]bool{}
	for _, revision := range revisions {
		excluded[recordIdentity(revision.OldTaskID, revision.OldRecordKey)] = true
	}
	out := make([]ChildResult, 0, len(rows))
	for _, row := range rows {
		current := row
		current.Records = []Record{}
		if row.Link.SourceID != "" {
			if err := requireActiveSourceVersion(s.Data.DB.WithContext(ctx), row.Link.SourceID, row.Link.ArtifactHash, false); err != nil {
				if !errors.Is(err, agentdata.ErrConflict) {
					return nil, err
				}
				current.OutputMetadata = nil
				out = append(out, current)
				continue
			}
		}
		for _, record := range row.Records {
			if !excluded[recordIdentity(row.Task.ID, record.Key)] {
				current.Records = append(current.Records, record)
			}
		}
		out = append(out, current)
	}
	return out, nil
}

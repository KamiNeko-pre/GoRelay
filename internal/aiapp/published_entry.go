package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	task "go-lua-crawler/internal/task"
	"gorm.io/gorm"
)

var ErrPublishedPending = errors.New("published acquisition has not delivered its frozen contract yet")

// Settled physical work can still fail the frozen delivery contract.
var ErrPublishedDeliveryMismatch = errors.New("published acquisition finished without satisfying its frozen delivery contract")

// DeliveredAcquisitionRecords uses the crawler's existing current-version and
// effective-child checks. Failed/superseded attempts are investigation history,
// not successful result rows for an upstream consumer to load blindly.
func (s *Service) DeliveredAcquisitionRecords(ctx context.Context, id string) ([]Record, error) {
	r, err := s.Data.Run(ctx, id)
	if err != nil {
		return nil, err
	}
	if r.Status != "completed" {
		return nil, ErrPublishedPending
	}
	result, err := s.Evaluate(ctx, r)
	if err != nil {
		return nil, err
	}
	if !taskDeliverySatisfied(&result) {
		return nil, fmt.Errorf("crawler delivery no longer satisfies its current contract: %v", result.Gaps)
	}
	children, err := s.ChildResults(ctx, id)
	if err != nil {
		return nil, err
	}
	children, err = s.effectiveChildren(ctx, id, children)
	if err != nil {
		return nil, err
	}
	rows := []Record{}
	for _, child := range children {
		rows = append(rows, child.Records...)
	}
	return rows, nil
}

// ResumePublishedCorrection gives the same Run back to the normal Lua loop
// after a published asset's physical execution has failed. Its contract,
// operation ledger and failed child remain available for actual correction.
func (s *Service) ResumePublishedCorrection(ctx context.Context, id string) (agentdata.Run, error) {
	r, err := s.Data.Run(ctx, id)
	if err != nil || r.IsTerminal() {
		return r, err
	}
	children, err := s.ChildResults(ctx, id)
	if err != nil {
		return r, err
	}
	if len(children) == 0 {
		return r, ErrPublishedPending
	}
	failed := false
	for _, child := range children {
		if !child.Task.Status.IsTerminal() {
			return r, ErrPublishedPending
		}
		failed = failed || child.Task.Status == task.StatusFailed
	}
	if !failed {
		result, err := s.Evaluate(ctx, r)
		if err != nil {
			return r, err
		}
		if taskDeliverySatisfied(&result) {
			return r, ErrPublishedPending
		}
	}
	if r.Status == "reviewing" {
		return r, nil
	}
	if r.Status == "asset_running" {
		r, err = s.Data.ClaimAssetRun(ctx, id, 2*time.Minute)
	} else {
		r, err = s.Data.Claim(ctx, id, 2*time.Minute)
	}
	if err != nil {
		return r, err
	}
	if err := s.Data.SaveRun(ctx, id, r.Token, map[string]any{"status": "reviewing", "next_wake_at": time.Now(), "last_error": "published acquisition did not satisfy its frozen delivery contract; continue normal Lua correction using its original child and tool receipts"}); err != nil {
		return r, err
	}
	return s.Data.Run(ctx, id)
}

// StartPublishedAcquisition is an application entry for an upstream router
// that already selected a published capability. It reuses the existing durable
// tool admission, Outbox, Worker, Runner and field checks. It does not alter the
// crawler's model loop or expose knowledge tools to that loop.
func (s *Service) StartPublishedAcquisition(ctx context.Context, in CreateRunRequest, source, version, target string, params map[string]any, deadline time.Time) (agentdata.Run, error) {
	in.ParentDeadline = &deadline
	if in.Spec == nil {
		return agentdata.Run{}, fmt.Errorf("published execution needs a frozen requested field contract")
	}
	if params == nil {
		params = map[string]any{}
	}
	var r agentdata.Run
	// Publish the run and its next wake atomically. The normal Agent scheduler
	// must not start a second model exploration for this already-selected work.
	err := s.Data.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		scoped := *s
		scoped.Data = agentdata.NewStore(tx)
		scoped.Knowledge = knowledge.NewStore(tx)
		var err error
		r, err = scoped.Create(ctx, in)
		if err != nil {
			return err
		}
		if r.IsTerminal() {
			return nil
		}
		end := r.Deadline
		if !deadline.IsZero() && deadline.Before(end) {
			end = deadline
		}
		if !end.IsZero() && !end.After(time.Now()) {
			return fmt.Errorf("published acquisition deadline reached")
		}
		var endValue any
		if !end.IsZero() {
			endValue = end
		}
		if err := tx.Model(&agentdata.Run{}).Where("id = ?", r.ID).Updates(map[string]any{"deadline": endValue, "next_wake_at": nil, "status": "asset_running"}).Error; err != nil {
			return err
		}
		r.Deadline = end
		r.Status = "asset_running"
		return nil
	})
	if err != nil {
		return r, err
	}
	links, err := s.Data.Children(ctx, r.ID)
	if err != nil {
		return r, err
	}
	if len(links) > 0 {
		if len(links) != 1 {
			return r, agentdata.ErrConflict
		}
		var child task.CrawlTask
		if err := s.Data.DB.WithContext(ctx).First(&child, "id = ?", links[0].TaskID).Error; err != nil {
			return r, err
		}
		var args childParameters
		if err := json.Unmarshal([]byte(child.ProgramParams), &args); err != nil {
			return r, err
		}
		if child.ProgramHash != version || child.URL != target || args.SourceID != source || agentdata.Hash(args.Params) != agentdata.Hash(params) {
			return r, agentdata.ErrConflict
		}
		return s.Data.Run(ctx, r.ID)
	}
	if r.IsTerminal() {
		return r, agentdata.ErrConflict
	}
	head, err := s.Data.SourceHead(ctx, source)
	if err != nil {
		return r, err
	}
	if head.Status != "active" || head.ArtifactHash != version {
		return r, fmt.Errorf("published source/version changed or was revoked; select the current catalog version")
	}
	r, err = s.Data.ClaimAssetRun(ctx, r.ID, 2*time.Minute)
	if err != nil {
		return r, err
	}
	call := ToolCall{ID: "host-published-acquisition", Type: "function", Function: FunctionCall{Name: "submit_acquisition", Arguments: string(mustMarshalPublished(map[string]any{"source_id": source, "url": target, "params": params}))}}
	_, cause := s.executeDurableTool(ctx, r, call)
	updates := map[string]any{"status": "asset_running", "next_wake_at": nil}
	if cause != nil {
		updates["last_error"] = cause.Error()
	}
	if err := s.Data.SaveRun(context.WithoutCancel(ctx), r.ID, r.Token, updates); err != nil {
		return r, err
	}
	if cause != nil {
		return r, cause
	}
	return s.Data.Run(ctx, r.ID)
}

func (s *Service) CompletePublishedAcquisition(ctx context.Context, id string) (agentdata.Run, error) {
	r, err := s.Data.Run(ctx, id)
	if err != nil || r.IsTerminal() {
		return r, err
	}
	r, err = s.Data.ClaimAssetRun(ctx, id, 2*time.Minute)
	if err != nil {
		return r, err
	}
	result, cause := s.Evaluate(ctx, r)
	updates := map[string]any{"status": "asset_running", "next_wake_at": nil}
	if cause == nil && taskDeliverySatisfied(&result) {
		updates["status"] = "completed"
		updates["result_json"] = string(mustMarshalPublished(result))
		updates["last_error"] = ""
	} else if cause == nil {
		cause = ErrPublishedPending
		children, err := s.ChildResults(ctx, id)
		if err != nil {
			cause = err
		} else if len(children) > 0 {
			settled := true
			for _, child := range children {
				settled = settled && child.Task.Status.IsTerminal()
			}
			if settled {
				cause = fmt.Errorf("%w: %v", ErrPublishedDeliveryMismatch, result.Gaps)
			}
		}
	}
	if err := s.Data.SaveRun(context.WithoutCancel(ctx), r.ID, r.Token, updates); err != nil {
		return r, err
	}
	current, err := s.Data.Run(ctx, id)
	if err != nil {
		return current, err
	}
	return current, cause
}

func mustMarshalPublished(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

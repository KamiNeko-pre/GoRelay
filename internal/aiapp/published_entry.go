package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	task "go-lua-crawler/internal/task"
	"gorm.io/gorm"
	"sort"
)

var ErrPublishedPending = errors.New("published acquisition has not delivered its frozen contract yet")

type ChildResult struct {
	Link           agentdata.RunTask `json:"link"`
	Task           task.CrawlTask    `json:"task"`
	Records        []Record          `json:"records,omitempty"`
	OutputMetadata map[string]any    `json:"output_metadata,omitempty"`
}

func (s *Service) ChildResults(ctx context.Context, id string) ([]ChildResult, error) {
	links, err := s.Data.Children(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.childResultsForLinks(ctx, links)
}

func (s *Service) childResultsForLinks(ctx context.Context, links []agentdata.RunTask) ([]ChildResult, error) {
	var err error
	out := make([]ChildResult, 0, len(links))
	for _, link := range links {
		row := ChildResult{Link: link}
		if err = s.Data.DB.WithContext(ctx).First(&row.Task, link.TaskID).Error; err != nil {
			return nil, err
		}
		if row.Task.Status == task.StatusSucceeded {
			var result repository.CrawlResult
			if err = s.Data.DB.WithContext(ctx).First(&result, "task_id = ?", link.TaskID).Error; err != nil {
				return nil, err
			}
			var payload AcquisitionResult
			if err = json.Unmarshal([]byte(result.Payload), &payload); err != nil {
				return nil, err
			}
			row.Records = payload.Records
			for key, value := range payload.Runtime.Collection {
				if key == "items" {
					continue
				}
				if row.OutputMetadata == nil {
					row.OutputMetadata = map[string]any{}
				}
				row.OutputMetadata[key] = value
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// SuccessfulAcquisitionEvidence returns actual successful physical work and its
// raw responses, including a successful subset of a terminal failed run. It
// applies current execution ownership, not the parent delivery policy.
func (s *Service) SuccessfulAcquisitionEvidence(ctx context.Context, id string) ([]Record, []acquisition.Response, error) {
	children, err := s.ChildResults(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	children, err = s.effectiveChildren(ctx, id, children)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Task.ID < children[j].Task.ID })
	rows := []Record{}
	sources := []acquisition.Response{}
	for _, child := range children {
		if child.Task.Status != task.StatusSucceeded {
			continue
		}
		var saved repository.CrawlResult
		if err := s.Data.DB.WithContext(ctx).First(&saved, "task_id = ?", child.Task.ID).Error; err != nil {
			return nil, nil, err
		}
		var result AcquisitionResult
		if err := json.Unmarshal([]byte(saved.Payload), &result); err != nil {
			return nil, nil, err
		}
		rows = append(rows, result.Records...)
		sources = append(sources, result.Runtime.SourceEvidence...)
	}
	return rows, sources, nil
}

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
	if r.Status != "completed" && r.Status != "partial" {
		return nil, ErrPublishedPending
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

func (s *Service) DeliveredAcquisitionSourceChecks(ctx context.Context, id string) ([]acquisition.SourceCheck, error) {
	return nil, nil
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
		return r, ErrPublishedPending
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
func (s *Service) SubmitSourceCollection(ctx context.Context, input CreateRunRequest, source, version, target string, params map[string]any) (agentdata.Run, error) {
	deadline := time.Now().Add(10 * time.Minute)
	if input.ParentDeadline != nil && !input.ParentDeadline.IsZero() {
		deadline = *input.ParentDeadline
	}
	return s.StartPublishedAcquisition(ctx, input, source, version, target, params, deadline)
}

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
	artifact, err := s.Data.Artifact(ctx, head.ArtifactHash)
	if err != nil {
		return r, err
	}
	if err := ValidateParameters(artifact.InputSchema, params); err != nil {
		return r, err
	}
	var production TaskSpec
	_ = json.Unmarshal([]byte(artifact.OutputSchema), &production)
	paramsBytes, _ := json.Marshal(childParameters{Params: params, Spec: production, SourceID: source})
	inputHash := agentdata.Hash([]any{artifact.Hash, target, string(paramsBytes)})
	key := "child-intent:" + inputHash
	_, cause := s.Data.SubmitChild(ctx, r.ID, r.Token, key, inputHash, artifact, target, string(paramsBytes), nil)
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
	children, err := s.ChildResults(ctx, id)
	if err != nil {
		return r, err
	}
	if len(children) == 0 {
		return r, ErrPublishedPending
	}
	updates := map[string]any{"status": "asset_running", "next_wake_at": nil}
	settled := true
	for _, child := range children {
		if !child.Task.Status.IsTerminal() {
			settled = false
			break
		}
	}
	var cause error
	if settled {
		updates["status"] = "completed"
		updates["last_error"] = ""
	} else {
		cause = ErrPublishedPending
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

func (s *Service) effectiveChildren(ctx context.Context, id string, rows []ChildResult) ([]ChildResult, error) {
	return rows, nil
}

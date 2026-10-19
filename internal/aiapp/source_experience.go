package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/task"
	"strings"
)

// Experiences are projections of durable observations, not model-authored facts.
type SourceExperience struct {
	RunID        string         `json:"run_id"`
	SourceID     string         `json:"source_id"`
	ArtifactHash string         `json:"artifact_hash"`
	CandidateID  string         `json:"candidate_id"`
	Failures     []TrialFailure `json:"prior_trial_failures"`
	SupportLevel string         `json:"support_level"`
}
type TrialFailure struct {
	CallID  string `json:"call_id"`
	URL     string `json:"url"`
	Purpose string `json:"purpose"`
	Error   string `json:"error"`
}

func (s *Service) sourceExperience(ctx context.Context, r agentdata.Run, id string) ([]SourceExperience, error) {
	out := []SourceExperience{}
	head, err := s.sourceHeadForTool(ctx, id)
	if err != nil {
		return out, err
	}
	if head.Status != "active" {
		return out, fmt.Errorf("source is not active")
	}
	allowed, err := s.canUseCapability(ctx, r, id, head.ArtifactHash)
	if err != nil {
		return out, err
	}
	if !allowed {
		return out, fmt.Errorf("capability reuse disabled")
	}
	artifact, err := s.Data.Artifact(ctx, head.ArtifactHash)
	if err != nil {
		return out, err
	}
	if err = checkCapabilityForRun(r, artifact); err != nil {
		return out, err
	}
	var candidates []Candidate
	if err = s.Data.DB.WithContext(ctx).Where("source_id = ? AND artifact_hash = ? AND run_id <> ?", id, head.ArtifactHash, r.ID).Order("created_at DESC").Limit(5).Find(&candidates).Error; err != nil {
		return out, err
	}
	for _, c := range candidates {
		var completed int64
		err = s.Data.DB.WithContext(ctx).Table("run_tasks l").Joins("JOIN crawl_tasks t ON t.id = l.task_id").Where("l.run_id = ? AND l.source_id = ? AND l.artifact_hash = ? AND t.status = ?", c.RunID, id, head.ArtifactHash, task.StatusSucceeded).Count(&completed).Error
		if err != nil {
			return out, err
		}
		if completed == 0 {
			continue
		}
		ops, err := s.Data.Operations(ctx, c.RunID)
		if err != nil {
			return out, err
		}
		calls := map[string]trialArgs{}
		for _, op := range ops {
			if op.Kind != "model" || op.Stage != "succeeded" {
				continue
			}
			var response ChatResponse
			if json.Unmarshal([]byte(op.ResultJSON), &response) != nil {
				continue
			}
			for _, call := range response.Message.ToolCalls {
				if call.Function.Name == "validate_program" {
					var a trialArgs
					if decodeArgs(call.Function.Arguments, &a) == nil {
						calls[call.ID] = a
					}
				}
			}
		}
		entry := SourceExperience{RunID: c.RunID, SourceID: id, ArtifactHash: head.ArtifactHash, CandidateID: c.ID, Failures: []TrialFailure{}, SupportLevel: "observed_trial_failure_then_validated_and_executed; causal_explanation_unverified"}
		for _, op := range ops {
			if op.Kind != "validate_program" || op.Stage != "failed" || op.CreatedAt.After(c.CreatedAt) {
				continue
			}
			callID := strings.TrimPrefix(op.Key, "tool:")
			a, ok := calls[callID]
			if !ok || a.SourceID != id {
				continue
			}
			if _, err := requireExperienceURL(a.URL, scope(r)); err != nil {
				continue
			}
			var receipt ToolResult
			if json.Unmarshal([]byte(op.ResultJSON), &receipt) != nil || receipt.Error == "" {
				continue
			}
			message := []rune(receipt.Error)
			if len(message) > 512 {
				message = message[:512]
			}
			entry.Failures = append(entry.Failures, TrialFailure{CallID: callID, URL: a.URL, Purpose: a.Purpose, Error: string(message)})
			if len(entry.Failures) == 5 {
				break
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

func requireExperienceURL(raw string, domains []string) (bool, error) {
	row := Record{Provenance: Provenance{URL: raw}}
	if !recordInScope(row, domains) {
		return false, fmt.Errorf("experience outside scope")
	}
	return true, nil
}

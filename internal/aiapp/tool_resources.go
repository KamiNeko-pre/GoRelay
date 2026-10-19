package aiapp

import (
	"context"
	"errors"
	"fmt"

	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
)

func (s *Service) candidateForTool(ctx context.Context, r agentdata.Run, id string) (Candidate, error) {
	var candidate Candidate
	err := s.Data.DB.WithContext(ctx).First(&candidate, "id = ? AND run_id = ?", id, r.ID).Error
	if err == nil {
		return candidate, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return candidate, fmt.Errorf("candidate lookup for candidate_id %q: %w", id, err)
	}
	var available []Candidate
	if err = s.Data.DB.WithContext(ctx).Where("run_id = ?", r.ID).Order("created_at desc").Limit(5).Find(&available).Error; err != nil {
		return candidate, fmt.Errorf("candidate inventory lookup: %w", err)
	}
	ids := make([]string, 0, len(available))
	for _, row := range available {
		ids = append(ids, row.ID)
	}
	return candidate, toolContractFailure(fmt.Errorf("candidate_id %q is not a validated program in this Run; available candidate ids, newest first: %v", id, ids), "CANDIDATE_UNAVAILABLE", "not_started", map[string]any{"field": "candidate_id", "requested_id": id, "available_candidate_ids": ids}, ToolRecovery{Strategy: "use_observed_reference", Tool: "validate_program", Instruction: "Use an exact candidate_id issued by validate_program(mode=acquisition) in this Run. If none exists, validate a program first. An adapter must be registered before submit_acquisition; direct candidate submission accepts a composition. Do not guess an ID or use a tool call ID."})
}

func (s *Service) sourceHeadForTool(ctx context.Context, id string) (agentdata.SourceHead, error) {
	head, err := s.Data.SourceHead(ctx, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return head, toolContractFailure(fmt.Errorf("source_id %q is not published", id), "SOURCE_UNAVAILABLE", "not_started", map[string]any{"field": "source_id", "requested_id": id}, ToolRecovery{Strategy: "use_observed_reference", Tool: "list_sources", Instruction: "Use a published source_id from list_sources or register_source. If only a validated adapter exists, register_source with its candidate_id first. Do not guess or substitute a tool call ID."})
	}
	if err != nil {
		return head, fmt.Errorf("source lookup for source_id %q: %w", id, err)
	}
	return head, nil
}

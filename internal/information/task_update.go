package information

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"gorm.io/gorm"
	"slices"
)

type UpdateInput struct {
	RequestKey  string    `json:"request_key"`
	Guidance    string    `json:"guidance,omitempty"`
	SourceScope *[]string `json:"source_scope,omitempty"`
}

// CreateUpdate starts a new user-requested business cycle. It reuses saved
// evidence as a baseline, never as proof of a fresh fetch.
func (s *Service) CreateUpdate(ctx context.Context, baselineID string, update UpdateInput) (Request, error) {
	var updated Request
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var baseline Delivery
		if err := tx.First(&baseline, "id = ?", baselineID).Error; err != nil {
			return err
		}
		var parent Request
		if err := tx.First(&parent, "id = ?", baseline.RequestID).Error; err != nil {
			return err
		}
		var input Input
		if err := json.Unmarshal([]byte(parent.InputJSON), &input); err != nil {
			return err
		}
		input.RequestKey = update.RequestKey
		input.Guidance = update.Guidance
		input.Refresh = true
		if update.SourceScope != nil {
			input.SourceScope = *update.SourceScope
		}
		local := *s
		local.DB = tx
		var err error
		updated, err = local.Create(ctx, input)
		if err != nil {
			return err
		}
		if updated.BaselineDeliveryID != "" {
			if updated.BaselineDeliveryID != baselineID {
				return agentdata.ErrConflict
			}
			return nil
		}
		var requirements TaskRequirements
		if err := json.Unmarshal([]byte(parent.RequirementsJSON), &requirements); err != nil {
			return err
		}
		requirements.Revision = 1
		requirements.SourceScope = input.SourceScope
		if update.Guidance != "" && !slices.Contains(requirements.Additions, update.Guidance) {
			requirements.Additions = append(requirements.Additions, update.Guidance)
		}
		if err := tx.Model(&Request{}).Where("id = ?", updated.ID).Updates(map[string]any{"baseline_delivery_id": baselineID, "requirements_json": string(mustMarshal(requirements)), "requirements_revision": 1}).Error; err != nil {
			return err
		}
		updated, err = local.Request(ctx, updated.ID)
		return err
	})
	return updated, err
}

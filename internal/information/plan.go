package information

import (
	"context"
	"encoding/json"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// UpdatePlan changes only the model's working interpretation and actions.
// No observations, user requirements, or existing delivery are invalidated.
func (s *Service) UpdatePlan(ctx context.Context, requestID string, plan WorkPlan) error {
	payload, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var request Request
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&request, "id = ?", requestID).Error; err != nil {
			return err
		}
		return tx.Model(&Request{}).Where("id = ?", requestID).Update("plan_json", string(payload)).Error
	})
}

func (s *Service) BuildView(ctx context.Context, requestID string, spec ViewSpec) (DeliveryTable, error) {
	if len(spec.ObservationIDs) == 0 {
		return DeliveryTable{Columns: spec.Columns, Rows: []map[string]any{}, RowObservationIDs: [][]string{}}, nil
	}
	page, err := s.QueryObservations(ctx, requestID, ObservationQuery{ObservationIDs: spec.ObservationIDs, Limit: len(spec.ObservationIDs)})
	if err != nil {
		return DeliveryTable{}, err
	}
	return projectObservations(page.Items, spec)
}

package agentdata

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Background index work may continue after a successful/partial delivery,
// while cancellation and operation ownership still apply.
func (s *Store) ReserveIndexOperation(ctx context.Context, id, key string) (Operation, bool, error) {
	var op Operation
	fresh := false
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r Run
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, "id = ?", id).Error; err != nil {
			return err
		}
		if r.Cancelled {
			return ErrConflict
		}
		err := tx.Where("run_id = ? AND `key` = ?", id, key).First(&op).Error
		if err == nil {
			if op.InputHash != Hash(key) || op.Kind != "index_embedding" {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		op = Operation{RunID: id, Key: key, InputHash: Hash(key), Kind: "index_embedding", Stage: "reserved"}
		if err = tx.Create(&op).Error; err != nil {
			return err
		}
		fresh = true
		return nil
	})
	return op, fresh, err
}

func (s *Store) CompleteIndexOperation(ctx context.Context, id uint64, stage string) error {
	if stage != "succeeded" && stage != "failed" {
		return ErrConflict
	}
	q := s.DB.WithContext(ctx).Model(&Operation{}).Where("id = ? AND kind = ? AND stage = ?", id, "index_embedding", "reserved").Update("stage", stage)
	if q.Error != nil {
		return q.Error
	}
	if q.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

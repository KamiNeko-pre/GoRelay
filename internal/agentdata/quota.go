package agentdata

import (
	"context"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

var ErrDailyQuota = errors.New("daily demo run quota exhausted")

type DailyAdmission struct {
	Day   string `gorm:"primaryKey;size:10"`
	Count int
}

// Admission limits new goals, not HTTP requests. A replay of the same immutable
// request remains readable after quota exhaustion. UTC is the billing day.
func (s *Store) CreateRunLimited(ctx context.Context, key, goal, input, budget string, deadline time.Time, limit int, now time.Time) (Run, error) {
	if limit <= 0 {
		return s.CreateRun(ctx, key, goal, input, budget, deadline)
	}
	var out Run
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		day := now.UTC().Format("2006-01-02")
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&DailyAdmission{Day: day}).Error; err != nil {
			return err
		}
		var quota DailyAdmission
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&quota, "day = ?", day).Error; err != nil {
			return err
		}
		var existing Run
		err := tx.Where("request_key = ?", key).First(&existing).Error
		if err == nil {
			if existing.Goal != goal || existing.InputJSON != input {
				return ErrConflict
			}
			out, err = NewStore(tx).RestoreSubmittedContract(ctx, existing.ID)
			return err
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if quota.Count >= limit {
			return ErrDailyQuota
		}
		out, err = NewStore(tx).CreateRun(ctx, key, goal, input, budget, deadline)
		if err != nil {
			return err
		}
		return tx.Model(&quota).Update("count", gorm.Expr("count + 1")).Error
	})
	return out, err
}

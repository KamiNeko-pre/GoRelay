package repository

import (
	"context"
	"fmt"
	"time"

	taskmodel "gorelay/internal/task"

	"gorm.io/gorm"
)

// PersistTaskResult writes one task result using the transaction supplied by
// CompleteCrawlTask. Keeping this as a callback avoids forcing the generic task
// package to depend on the current GitHub-specific result model.
type PersistTaskResult func(tx *gorm.DB) error

// CompleteCrawlTask is the transaction boundary required before Redis XACK.
func CompleteCrawlTask(
	ctx context.Context,
	taskID uint64,
	to taskmodel.Status,
	failureCode string,
	lastError string,
	finishedAt time.Time,
	runToken string,
	persistResult PersistTaskResult,
) error {
	if taskID == 0 {
		return fmt.Errorf("complete task: task ID must be positive")
	}
	if !to.IsTerminal() {
		return fmt.Errorf("%w: running -> %s", ErrInvalidTaskTransition, to)
	}
	if to == taskmodel.StatusSucceeded && persistResult == nil {
		return fmt.Errorf("complete task %d: success requires a result persistence callback", taskID)
	}
	if runToken == "" {
		return fmt.Errorf("complete task %d: execution token is required", taskID)
	}

	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{
			"status":       to,
			"failure_code": failureCode,
			"last_error":   lastError,
			"finished_at":  finishedAt,
			"lease_until":  nil,
		}

		result := tx.Model(&taskmodel.CrawlTask{}).Where("id=? AND status = ? AND run_token = ?", taskID, taskmodel.StatusRunning, runToken).Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("complete task %d: update terminal state: %w", taskID, result.Error)
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("complete task %d: %w", taskID, ErrTaskTransitionConflict)
		}

		if to == taskmodel.StatusSucceeded {
			err := persistResult(tx)
			if err != nil {
				return fmt.Errorf("complete task %d: persist result: %w", taskID, err)
			}
		}
		return nil
	})
}

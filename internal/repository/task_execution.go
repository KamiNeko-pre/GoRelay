package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	taskmodel "gorelay/internal/task"
	"gorm.io/gorm"
)

// ClaimCrawlTask grants a bounded attempt; PEL transfer alone is not ownership.
func ClaimCrawlTask(ctx context.Context, task taskmodel.CrawlTask, lease time.Duration, reclaim bool) (string, error) {
	if task.ID == 0 || lease <= 0 {
		return "", fmt.Errorf("claim requires a task ID and positive lease")
	}
	var tokenBytes [16]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(tokenBytes[:])
	now := time.Now()
	updates := map[string]any{"started_at": now, "lease_until": now.Add(lease), "run_token": token}
	if task.Status == taskmodel.StatusQueued {
		if err := TransitionCrawlTask(ctx, task.ID, taskmodel.StatusQueued, taskmodel.StatusRunning, updates); err != nil {
			return "", err
		}
		return token, nil
	}
	if !reclaim || task.Status != taskmodel.StatusRunning {
		return "", ErrTaskTransitionConflict
	}
	updates["retry_count"] = gorm.Expr("retry_count + 1")
	result := DB.WithContext(ctx).Model(&taskmodel.CrawlTask{}).
		Where("id = ? AND status = ? AND run_token = ? AND lease_until <= ?", task.ID, taskmodel.StatusRunning, task.RunToken, now).Updates(updates)
	if result.Error != nil {
		return "", fmt.Errorf("reclaim task %d: %w", task.ID, result.Error)
	}
	if result.RowsAffected != 1 {
		return "", ErrTaskTransitionConflict
	}
	return token, nil
}

// The task ID primary key gives each accepted task one immutable result snapshot.
type CrawlResult struct {
	TaskID    uint64    `json:"task_id" gorm:"primaryKey;autoIncrement:false"`
	Payload   string    `json:"payload" gorm:"type:json;not null"`
	CreatedAt time.Time `json:"created_at"`
}

// Renewal preserves the same execution token; only a live, uncancelled owner may extend it.
func RenewCrawlTaskLease(ctx context.Context, id uint64, token string, lease time.Duration, _ string) error {
	now := time.Now()
	result := DB.WithContext(ctx).Model(&taskmodel.CrawlTask{}).
		Where("id = ? AND status = ? AND run_token = ? AND cancelled = ? AND lease_until > ?", id, taskmodel.StatusRunning, token, false, now).
		Update("lease_until", now.Add(lease))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrTaskTransitionConflict
	}
	return nil
}

// CancelCrawlTask marks a task cancelled and immediately finalizes pending/queued rows.
func CancelCrawlTask(ctx context.Context, db *gorm.DB, id uint64) (taskmodel.CrawlTask, error) {
	if db == nil {
		db = DB
	}
	var out taskmodel.CrawlTask
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&out, id).Error; err != nil {
			return err
		}
		if out.Status.IsTerminal() {
			return nil
		}
		now := time.Now()
		updates := map[string]any{"cancelled": true}
		if out.Status == taskmodel.StatusPending || out.Status == taskmodel.StatusQueued {
			updates["status"] = taskmodel.StatusCancelled
			updates["finished_at"] = now
			updates["lease_until"] = nil
		}
		if err := tx.Model(&taskmodel.CrawlTask{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return err
		}
		return tx.First(&out, id).Error
	})
	return out, err
}

// UpdateCrawlTaskCheckpointCAS atomically advances checkpoint_json and records_saved
// only if the caller holds the active non-cancelled fencing token.
func UpdateCrawlTaskCheckpointCAS(ctx context.Context, tx *gorm.DB, id uint64, token string, checkpointJSON string, recordsDelta int) error {
	if tx == nil {
		tx = DB
	}
	if id == 0 || token == "" {
		return fmt.Errorf("checkpoint CAS requires task ID and fencing token")
	}
	result := tx.WithContext(ctx).Model(&taskmodel.CrawlTask{}).
		Where("id = ? AND status = ? AND run_token = ? AND cancelled = ?", id, taskmodel.StatusRunning, token, false).
		Updates(map[string]any{
			"checkpoint_json": checkpointJSON,
			"records_saved":   gorm.Expr("records_saved + ?", recordsDelta),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrTaskTransitionConflict
	}
	return nil
}

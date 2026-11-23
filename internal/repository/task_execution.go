package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	taskmodel "go-lua-crawler/internal/task"
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

// Renewal preserves the same execution token; only a live owner may extend it.
func RenewCrawlTaskLease(ctx context.Context, id uint64, token string, lease time.Duration, runID string) error {
	now := time.Now()
	query := DB.WithContext(ctx).Model(&taskmodel.CrawlTask{}).
		Where("id = ? AND status = ? AND run_token = ? AND lease_until > ?", id, taskmodel.StatusRunning, token, now)
	if runID != "" {
		query = query.Where("EXISTS (SELECT 1 FROM agent_runs WHERE agent_runs.id = ? AND cancelled = ? AND status NOT IN ? AND (deadline IS NULL OR deadline > ?))", runID, false, []string{"completed", "partial", "failed", "cancelled", "expired"}, now)
	}
	result := query.Update("lease_until", now.Add(lease))
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrTaskTransitionConflict
	}
	return nil
}

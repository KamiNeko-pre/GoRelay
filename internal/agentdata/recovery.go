package agentdata

import (
	"context"
	task "go-lua-crawler/internal/task"
	"time"
)

// ReconcileQueue is for an isolated restored deployment with all publishers and
// workers stopped. Redis is derived; task snapshots, cancellation and budgets
// stay unchanged. Redelivery may duplicate IDs; worker token checks own effects.
func (s *Store) ReconcileQueue(ctx context.Context, now time.Time) (int64, error) {
	eligible := s.DB.WithContext(ctx).Table("crawl_tasks t").Select("t.id").Joins("LEFT JOIN agent_runs r ON r.id = t.run_id").Where("t.status IN ?", []task.Status{task.StatusQueued, task.StatusRunning}).Where("t.run_id = '' OR (r.id IS NOT NULL AND r.cancelled = ? AND (r.deadline IS NULL OR r.deadline > ?) AND r.status IN ?)", false, now, []string{"planning", "running", "waiting", "reviewing", "blocked_core", "blocked_configuration", "needs_input"})
	q := s.DB.WithContext(ctx).Model(&task.TaskOutbox{}).Where("task_id IN (?) AND published_at IS NOT NULL", eligible).Updates(map[string]any{"published_at": nil, "message_id": ""})
	return q.RowsAffected, q.Error
}

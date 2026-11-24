package repository

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	taskmodel "gorelay/internal/task"
)

// RecordTaskEvent writes one diagnostic milestone without changing task state.
func RecordTaskEvent(ctx context.Context, event *taskmodel.TaskEvent) error {
	if DB == nil {
		return fmt.Errorf("record task event: database is not initialized")
	}
	if event == nil {
		return fmt.Errorf("record task event: event is required")
	}
	if event.TaskID == 0 {
		return fmt.Errorf("record task event: task ID must be positive")
	}
	if strings.TrimSpace(event.Stage) == "" {
		return fmt.Errorf("record task event: stage is required")
	}
	if strings.TrimSpace(event.Level) == "" {
		return fmt.Errorf("record task event: level is required")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	if err := DB.WithContext(ctx).Create(event).Error; err != nil {
		return fmt.Errorf("record task event %d/%s: %w", event.TaskID, event.Stage, err)
	}
	return nil
}

// RecordTaskEventBestEffort keeps diagnostics outside the task success path.
// An unavailable event table must not turn a successfully completed crawl into
// a failed crawl, so the write is bounded and its error is logged only.
func RecordTaskEventBestEffort(ctx context.Context, event taskmodel.TaskEvent) {
	if DB == nil || event.TaskID == 0 {
		return
	}
	eventCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if err := RecordTaskEvent(eventCtx, &event); err != nil {
		log.Printf("record task event %d/%s: %v", event.TaskID, event.Stage, err)
	}
}

// ListTaskEvents returns a stable chronological trace for one task.
func ListTaskEvents(ctx context.Context, taskID uint64) ([]taskmodel.TaskEvent, error) {
	if DB == nil {
		return nil, fmt.Errorf("list task events: database is not initialized")
	}
	if taskID == 0 {
		return nil, fmt.Errorf("list task events: task ID must be positive")
	}
	var events []taskmodel.TaskEvent
	if err := DB.WithContext(ctx).
		Where("task_id = ?", taskID).
		Order("created_at ASC").
		Order("id ASC").
		Find(&events).Error; err != nil {
		return nil, fmt.Errorf("list task events %d: %w", taskID, err)
	}
	return events, nil
}

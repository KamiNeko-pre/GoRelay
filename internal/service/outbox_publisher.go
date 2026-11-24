package service

import (
	"context"
	"fmt"
	"time"

	"gorelay/internal/queue"
	"gorelay/internal/repository"
	taskmodel "gorelay/internal/task"
)

// PublishPendingTaskOutboxes publishes the task IDs from one bounded Outbox
// batch. Its count includes only records marked published by this call.
// Retries can deliver duplicates and require downstream idempotency.
func PublishPendingTaskOutboxes(ctx context.Context, limit int) (int, error) {
	outboxes, err := repository.ListUnpublishedTaskOutboxes(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("load outbox batch (limit %d): %w", limit, err)
	}
	publishedCount := 0
	for _, outbox := range outboxes {
		messageID, err := queue.PublishTask(ctx, outbox.TaskID)
		if err != nil {
			return publishedCount, fmt.Errorf("publish outbox %d for task %d: %w", outbox.ID, outbox.TaskID, err)
		}
		if err := repository.MarkTaskOutboxPublished(ctx, outbox.ID, messageID, time.Now()); err != nil {
			return publishedCount, fmt.Errorf("mark outbox %d published (message %q): %w", outbox.ID, messageID, err)
		}
		repository.RecordTaskEventBestEffort(ctx, taskmodel.TaskEvent{
			TaskID:  outbox.TaskID,
			Stage:   taskmodel.EventStagePublished,
			Level:   taskmodel.EventLevelInfo,
			Message: fmt.Sprintf("已写入 Redis Stream，消息 ID 为 %s", messageID),
		})
		publishedCount++
	}
	return publishedCount, nil
}

package repository

import (
	"context"
	"errors"
	"fmt"
	taskmodel "go-lua-crawler/internal/task"
	"gorm.io/gorm"
	"time"
)

var (
	ErrInvalidTaskTransition  = errors.New("invalid task transition")
	ErrTaskTransitionConflict = errors.New("task transition conflict")
)

func TransitionCrawlTask(
	ctx context.Context,
	id uint64,
	from taskmodel.Status,
	to taskmodel.Status,
	extraUpdates map[string]any,
) error {

	if !taskmodel.CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTaskTransition, from, to)
	}
	updates := map[string]any{
		"status": to,
	}
	for key, value := range extraUpdates {
		updates[key] = value
	}
	result := DB.WithContext(ctx).Model(&taskmodel.CrawlTask{}).Where("id=? AND status=?", id, from).Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("transition crawl task: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrTaskTransitionConflict
	}
	return nil
}

func CreateCrawlTask(ctx context.Context, crawlTask *taskmodel.CrawlTask) error {
	if err := DB.WithContext(ctx).Create(crawlTask).Error; err != nil {
		return fmt.Errorf("create crawl task: %w", err)
	}
	return nil
}

// CreateQueuedCrawlTaskWithOutbox atomically creates a queued task and its
// unpublished Outbox record.
func CreateQueuedCrawlTaskWithOutbox(
	ctx context.Context,
	crawlTask *taskmodel.CrawlTask,
) error {
	if crawlTask == nil {
		return fmt.Errorf("crawl task is required")
	}
	queuedAt := time.Now()
	crawlTask.Status = taskmodel.StatusQueued
	crawlTask.QueuedAt = &queuedAt
	return DB.WithContext(ctx).Transaction(
		func(tx *gorm.DB) error {
			if err := snapshotKnownRule(ctx, tx, crawlTask); err != nil {
				return fmt.Errorf("snapshot accepted rule: %w", err)
			}
			if err := tx.Create(crawlTask).Error; err != nil {
				return fmt.Errorf("create queued crawl task: %w", err)
			}
			var outbox taskmodel.TaskOutbox
			outbox.TaskID = crawlTask.ID
			if err := tx.Create(&outbox).Error; err != nil {
				return fmt.Errorf("create task outbox: %w", err)
			}
			return nil
		},
	)
}

func GetCrawlTask(ctx context.Context, id uint64) (taskmodel.CrawlTask, error) {
	var crawlTask taskmodel.CrawlTask
	if err := DB.WithContext(ctx).First(&crawlTask, id).Error; err != nil {
		return taskmodel.CrawlTask{}, fmt.Errorf("get crawl task: %w", err)
	}
	return crawlTask, nil
}

func GetCrawlTaskByRequestKey(ctx context.Context, requestKey string) (taskmodel.CrawlTask, error) {
	var crawlTask taskmodel.CrawlTask
	if err := DB.WithContext(ctx).Where("request_key = ?", requestKey).First(&crawlTask).Error; err != nil {
		return taskmodel.CrawlTask{}, fmt.Errorf("get crawl task by request key: %w", err)
	}
	return crawlTask, nil
}

func IsCrawlTaskNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

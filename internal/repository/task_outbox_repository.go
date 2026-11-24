package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	taskmodel "gorelay/internal/task"
)

var (
	ErrInvalidOutboxBatchSize = errors.New("outbox batch size must be positive")
	ErrOutboxAlreadyPublished = errors.New("task outbox was already published")
)

// ListUnpublishedTaskOutboxes returns a stable batch of records that still
// need Redis delivery.
func ListUnpublishedTaskOutboxes(
	ctx context.Context,
	limit int,
) ([]taskmodel.TaskOutbox, error) {
	if limit <= 0 {
		return nil, ErrInvalidOutboxBatchSize
	}
	var outboxes []taskmodel.TaskOutbox
	if err := DB.WithContext(ctx).Where("published_at IS NULL").Order("id ASC").Limit(limit).Find(&outboxes).Error; err != nil {
		return nil, fmt.Errorf("list unpublished task outboxes: %w", err)
	}
	return outboxes, nil
}

// MarkTaskOutboxPublished records one successful XADD. The compare-and-set
// update prevents simultaneous Publisher instances from both marking the same
// row as newly published.
func MarkTaskOutboxPublished(
	ctx context.Context,
	outboxID uint64,
	messageID string,
	publishedAt time.Time,
) error {
	if outboxID == 0 {
		return fmt.Errorf("outbox ID must be positive")
	}
	if messageID == "" {
		return fmt.Errorf("message ID must not be empty")
	}

	updates := map[string]any{
		"message_id":   messageID,
		"published_at": publishedAt,
	}
	result := DB.WithContext(ctx).
		Model(&taskmodel.TaskOutbox{}).
		Where("id = ? AND published_at IS NULL", outboxID).
		Updates(updates)
	if result.Error != nil {
		return fmt.Errorf("mark task outbox published: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrOutboxAlreadyPublished
	}
	return nil
}

package queue

import (
	"context"
	"fmt"

	"gorelay/internal/config"

	"github.com/redis/go-redis/v9"
)

func PublishTask(ctx context.Context, taskID uint64) (string, error) {
	if Client == nil {
		return "", fmt.Errorf("redis client is not initialized")
	}

	streamName := config.Get().Redis.Stream
	messageID, err := Client.XAdd(ctx, &redis.XAddArgs{
		Stream: streamName,
		Values: map[string]any{
			"task_id": taskID,
		},
	}).Result()
	if err != nil {
		return "", fmt.Errorf("publish task: %w", err)
	}
	return messageID, nil
}

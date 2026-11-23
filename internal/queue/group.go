package queue

import (
	"context"
	"fmt"
	"strings"

	"gorelay/internal/config"

	"github.com/redis/go-redis/v9"
)

// EnsureConsumerGroup creates the configured Stream and Consumer Group when
// they do not exist. Repeated calls are safe because BUSYGROUP means the group
// is already ready for use.
func EnsureConsumerGroup(ctx context.Context) error {
	if Client == nil {
		return fmt.Errorf("redis client is not initialized")
	}

	cfg := config.Get()
	if cfg == nil {
		return fmt.Errorf("configuration is not initialized")
	}
	stream := strings.TrimSpace(cfg.Redis.Stream)
	group := strings.TrimSpace(cfg.Redis.ConsumerGroup)
	if stream == "" {
		return fmt.Errorf("redis stream is required")
	}
	if group == "" {
		return fmt.Errorf("redis consumer group is required")
	}

	err := Client.XGroupCreateMkStream(ctx, stream, group, "0").Err()
	if err == nil || redis.HasErrorPrefix(err, "BUSYGROUP") {
		return nil
	}
	return fmt.Errorf("create Redis consumer group: %w", err)
}

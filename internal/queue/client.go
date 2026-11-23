package queue

import (
	"context"
	"fmt"
	"time"

	"gorelay/internal/config"

	"github.com/redis/go-redis/v9"
)

var Client *redis.Client

func InitClient(ctx context.Context) error {
	cfg := config.Get().Redis
	client := redis.NewClient(&redis.Options{
		Addr:                  cfg.Addr,
		Password:              cfg.Password,
		DB:                    cfg.DB,
		ContextTimeoutEnabled: true,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return fmt.Errorf("ping redis: %w", err)
	}

	Client = client
	return nil
}

func Close() error {
	if Client == nil {
		return nil
	}
	return Client.Close()
}

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gorelay/internal/config"
	"gorelay/internal/logger"
	"gorelay/internal/queue"
	"gorelay/internal/repository"
	"gorelay/internal/service"
	"go.uber.org/zap"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	config.InitConfig()
	logger.Init()
	defer func() { _ = logger.Log.Sync() }()
	if err := repository.InitDB(); err != nil {
		return fmt.Errorf("initialize MySQL: %w", err)
	}
	sqlDB, err := repository.DB.DB()
	if err != nil {
		return fmt.Errorf("get MySQL pool: %w", err)
	}
	defer sqlDB.Close()
	if err := queue.InitClient(ctx); err != nil {
		return fmt.Errorf("initialize Redis: %w", err)
	}
	defer queue.Close()
	return runPublisherLoop(ctx, config.Get().Publisher, service.PublishPendingTaskOutboxes, logger.Log)
}

// Batches run serially. A failed batch leaves unpublished rows for the next tick.
func runPublisherLoop(
	ctx context.Context,
	cfg config.PublisherConfig,
	publish func(context.Context, int) (int, error),
	log *zap.Logger,
) error {
	if cfg.BatchSize <= 0 || cfg.PollIntervalMS <= 0 || cfg.BatchTimeoutSeconds <= 0 {
		return fmt.Errorf("publisher batch size, interval and timeout must be positive")
	}
	ticker := time.NewTicker(time.Duration(cfg.PollIntervalMS) * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return nil
		}
		batchCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.BatchTimeoutSeconds)*time.Second)
		count, err := publish(batchCtx, cfg.BatchSize)
		cancel()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			log.Error("Outbox batch failed; unpublished rows will be retried", zap.Int("published_count", count), zap.Error(err))
		} else if count > 0 {
			log.Info("Outbox batch published", zap.Int("published_count", count))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

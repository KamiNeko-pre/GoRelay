package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/config"
	"go-lua-crawler/internal/logger"
	"go-lua-crawler/internal/queue"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/worker"

	"go.uber.org/zap"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Log.Fatal("Redis Worker exited", zap.Error(err))
	}
}

func run(ctx context.Context) error {
	config.InitConfig()
	logger.Init()
	defer func() {
		if err := logger.Log.Sync(); err != nil {
			fmt.Fprintf(os.Stderr, "flush logger: %v\n", err)
		}
	}()

	if err := repository.InitDB(); err != nil {
		return fmt.Errorf("initialize MySQL: %w", err)
	}
	sqlDB, err := repository.DB.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	ai := aiapp.NewService(repository.DB)
	if err := ai.Migrate(); err != nil {
		return err
	}
	ai.Configure()
	startPprofServer(config.Get().App.PprofPort)
	if err := queue.InitClient(ctx); err != nil {
		return fmt.Errorf("initialize Redis: %w", err)
	}
	defer func() {
		if err := queue.Close(); err != nil {
			logger.Log.Error("close Redis client", zap.Error(err))
		}
	}()
	if err := queue.EnsureConsumerGroup(ctx); err != nil {
		return fmt.Errorf("ensure Redis consumer group: %w", err)
	}

	cfg := config.Get()
	if cfg.Engine.WorkerCount < 1 || cfg.Engine.LuaTimeout < 1 {
		return fmt.Errorf("worker count and Lua timeout must be positive")
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, cfg.Engine.WorkerCount)
	for i := 0; i < cfg.Engine.WorkerCount; i++ {
		workerConfig := worker.DefaultConfig(
			cfg.Redis.Stream,
			cfg.Redis.ConsumerGroup,
			fmt.Sprintf("%s-%d", consumerName(), i),
		)
		workerConfig.ExecutionTimeout = time.Duration(cfg.Engine.LuaTimeout) * time.Second
		workerConfig.ClaimMinIdle = time.Duration(cfg.Redis.ClaimMinIdleMS) * time.Millisecond
		workerConfig.ClaimInterval = time.Duration(cfg.Redis.ClaimIntervalMS) * time.Millisecond
		processor, err := worker.NewProcessor(queue.Client, workerConfig, ai.ExecuteTask)
		if err != nil {
			return fmt.Errorf("create processor: %w", err)
		}
		consumer, err := worker.NewConsumer(queue.Client, workerConfig, processor)
		if err != nil {
			return fmt.Errorf("create consumer: %w", err)
		}
		reclaimer, err := worker.NewReclaimer(queue.Client, workerConfig, processor)
		if err != nil {
			return fmt.Errorf("create reclaimer: %w", err)
		}
		service, err := worker.NewService(consumer, reclaimer)
		if err != nil {
			return fmt.Errorf("create worker service: %w", err)
		}

		go func() { done <- service.Run(child) }()
	}
	first := <-done
	cancel()
	for i := 1; i < cfg.Engine.WorkerCount; i++ {
		<-done
	}
	return first
}

func consumerName() string {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "unknown-host"
	}
	return fmt.Sprintf("%s-%d", hostname, os.Getpid())
}

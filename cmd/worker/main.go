package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorelay/internal/config"
	"gorelay/internal/connector"
	"gorelay/internal/fetchgov"
	"gorelay/internal/logger"
	"gorelay/internal/pipeline"
	"gorelay/internal/queue"
	"gorelay/internal/repository"
	"gorelay/internal/worker"
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

	repository.ExtraMigrator = func(db *gorm.DB) error {
		if err := pipeline.Migrate(db); err != nil {
			return err
		}
		return connector.Migrate(db)
	}
	if err := repository.InitDB(); err != nil {
		return fmt.Errorf("initialize MySQL: %w", err)
	}
	sqlDB, err := repository.DB.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()

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

	govCfg, err := fetchgov.ConfigFromEnv()
	if err != nil {
		return fmt.Errorf("load SafeClient config: %w", err)
	}
	safeClient, err := fetchgov.NewSafeClient(govCfg)
	if err != nil {
		return fmt.Errorf("init SafeClient: %w", err)
	}
	defer safeClient.Close()

	snapDir := strings.TrimSpace(os.Getenv("GORELAY_SNAPSHOT_DIR"))
	connSvc, err := connector.NewService(repository.DB, safeClient, pipeline.NewStore(repository.DB, snapDir), connector.ServiceConfig{
		RunnerURL:         strings.TrimSpace(os.Getenv("GORELAY_RUNNER_URL")),
		RunnerToken:       strings.TrimSpace(os.Getenv("GORELAY_RUNNER_TOKEN")),
		CheckpointBaseURL: strings.TrimSpace(os.Getenv("GORELAY_CHECKPOINT_BASE_URL")),
		RespectRobots:     govCfg.RespectRobots,
		SnapshotDir:       snapDir,
	})
	if err != nil {
		return fmt.Errorf("init connector service: %w", err)
	}

	if cpListen := strings.TrimSpace(os.Getenv("GORELAY_CHECKPOINT_LISTEN")); cpListen != "" {
		cpSrv := &http.Server{Addr: cpListen, Handler: connSvc.Hub, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			<-ctx.Done()
			cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = cpSrv.Shutdown(cctx)
		}()
		go func() {
			if err := cpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Log.Error("checkpoint callback server exited", zap.Error(err))
			}
		}()
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
		processor, err := worker.NewProcessor(queue.Client, workerConfig, connSvc.ExecuteConnectorTask)
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
		svc, err := worker.NewService(consumer, reclaimer)
		if err != nil {
			return fmt.Errorf("create worker service: %w", err)
		}
		go func() {
			done <- svc.Run(child)
		}()
	}

	var firstErr error
	for i := 0; i < cfg.Engine.WorkerCount; i++ {
		if err := <-done; err != nil && firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	return firstErr
}

func consumerName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "worker"
	}
	return host
}

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
	"gorelay/internal/repository"
	"gorelay/internal/router"
)

func main() {
	config.InitConfig()
	logger.Init()
	defer func() { _ = logger.Log.Sync() }()
	startPprofServer(config.Get().App.PprofPort)

	repository.ExtraMigrator = func(db *gorm.DB) error {
		if err := pipeline.Migrate(db); err != nil {
			return err
		}
		return connector.Migrate(db)
	}
	if err := repository.InitDB(); err != nil {
		logger.Log.Fatal("数据库初始化失败", zap.Error(err))
	}

	govCfg, err := fetchgov.ConfigFromEnv()
	if err != nil {
		logger.Log.Fatal("SafeClient config failed", zap.Error(err))
	}
	safeClient, err := fetchgov.NewSafeClient(govCfg)
	if err != nil {
		logger.Log.Fatal("SafeClient initialization failed", zap.Error(err))
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
		logger.Log.Fatal("Connector service initialization failed", zap.Error(err))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if strings.EqualFold(os.Getenv("GORELAY_CANARY_ENABLED"), "true") {
		connSvc.StartHotCanaryMonitor(ctx, 15*time.Minute, 14*24*time.Hour, 6*time.Hour)
	}

	r := router.SetupRouter(connSvc)
	addr := fmt.Sprintf(":%d", config.Get().App.Port)
	logger.Log.Info("GoRelay MCP server started", zap.String("addr", addr))
	server := &http.Server{Addr: addr, Handler: r, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Log.Fatal("服务器启动失败", zap.Error(err))
	}
}

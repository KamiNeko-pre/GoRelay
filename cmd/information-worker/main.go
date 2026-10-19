package main

import (
	"context"
	"errors"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/config"
	"go-lua-crawler/internal/information"
	"go-lua-crawler/internal/logger"
	"go-lua-crawler/internal/repository"
	"go.uber.org/zap"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	config.InitConfig()
	logger.Init()
	defer logger.Log.Sync()
	if err := repository.InitDB(); err != nil {
		logger.Log.Fatal("information database failed", zap.Error(err))
	}
	crawler := aiapp.NewService(repository.DB)
	if err := crawler.Migrate(); err != nil {
		logger.Log.Fatal("crawler schema failed", zap.Error(err))
	}
	crawler.Configure()
	service := information.NewService(crawler)
	if err := service.Configure(); err != nil {
		logger.Log.Fatal("information configuration failed", zap.Error(err))
	}
	if err := service.Migrate(); err != nil {
		logger.Log.Fatal("information schema failed", zap.Error(err))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Log.Fatal("information worker stopped", zap.Error(err))
	}
}

// agent-worker owns Agent scheduling independently of the HTTP API process.
package main

import (
	"context"
	"errors"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/config"
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
		logger.Log.Fatal("Agent worker database failed", zap.Error(err))
	}
	s := aiapp.NewService(repository.DB)
	if err := s.Migrate(); err != nil {
		logger.Log.Fatal("Agent migration failed", zap.Error(err))
	}
	s.Configure()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := s.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Log.Fatal("Agent worker stopped", zap.Error(err))
	}
}

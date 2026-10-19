package main

import (
	"context"
	"errors"
	"fmt"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/config"
	"go-lua-crawler/internal/information"
	"go-lua-crawler/internal/logger"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/router"
	"go.uber.org/zap"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	// 启动顺序很重要：后续组件都会读取配置或使用日志、数据库等全局依赖。
	// 因此先加载配置，再依次初始化基础设施和业务组件。
	// 加载 configs/config.yaml，并启动配置文件监听。
	config.InitConfig()
	// 初始化全局 zap 日志。defer 保证进程退出前尽量刷出缓冲日志。
	logger.Init()
	defer logger.Log.Sync()
	startPprofServer(config.Get().App.PprofPort)
	// API 进程只负责受理任务和提供查询接口；发布、消费分别由独立进程处理。
	if err := repository.InitDB(); err != nil {
		logger.Log.Fatal("数据库初始化失败", zap.Error(err))
	}
	// 组装 HTTP 路由后，Gin 开始监听并阻塞主 goroutine。
	ai := aiapp.NewService(repository.DB)
	if err := ai.Migrate(); err != nil {
		logger.Log.Fatal("Agent migration failed", zap.Error(err))
	}
	ai.Configure()
	info := information.NewService(ai)
	if err := info.Configure(); err != nil {
		logger.Log.Warn("information frontend unavailable", zap.Error(err))
		info = nil
	} else if err := info.Migrate(); err != nil {
		logger.Log.Warn("information schema unavailable", zap.Error(err))
		info = nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := ai.PreflightModel(ctx); err != nil {
		logger.Log.Warn("Agent model backend not ready; new runs will be rejected", zap.Error(err))
	}
	r := router.SetupRouterWithInformation(ai, info)

	addr := fmt.Sprintf(":%d", config.Get().App.Port)
	logger.Log.Info("===========爬虫系统启动成功========", zap.String("监听地址", addr))
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

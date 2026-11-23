package main

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"

	"gorelay/internal/logger"
	"go.uber.org/zap"
)

func startPprofServer(port int) {
	if port <= 0 {
		return
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	go func() {
		logger.Log.Info("pprof diagnostics enabled", zap.String("address", addr))
		if err := http.ListenAndServe(addr, nil); err != nil {
			logger.Log.Error("pprof diagnostics stopped", zap.Error(err))
		}
	}()
}

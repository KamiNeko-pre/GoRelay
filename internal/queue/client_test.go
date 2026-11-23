package queue

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gorelay/internal/config"
)

func TestInitClientIntegration(t *testing.T) {
	if os.Getenv("REDIS_INTEGRATION") != "1" {
		t.Skip("REDIS_INTEGRATION is not set to 1")
	}

	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	t.Chdir(filepath.Clean(filepath.Join(workingDir, "..", "..")))
	config.InitConfig()
	if err := InitClient(context.Background()); err != nil {
		t.Fatalf("initialize Redis client: %v", err)
	}
	t.Cleanup(func() {
		if err := Close(); err != nil {
			t.Errorf("close Redis client: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Client.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping Redis: %v", err)
	}
}

package queue

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gorelay/internal/config"
)

func TestPublishTask(t *testing.T) {
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

	taskID := uint64(time.Now().UnixNano())
	messageID, err := PublishTask(context.Background(), taskID)
	if err != nil {
		t.Fatalf("publish task: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := Client.XDel(ctx, config.Get().Redis.Stream, messageID).Err(); err != nil {
			t.Errorf("clean up stream message: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	entries, err := Client.XRevRangeN(ctx, config.Get().Redis.Stream, "+", "-", 20).Result()
	if err != nil {
		t.Fatalf("read stream entries: %v", err)
	}

	for _, entry := range entries {
		if fmt.Sprint(entry.Values["task_id"]) == fmt.Sprint(taskID) {
			return
		}
	}
	t.Fatalf("published task %d was not found in stream", taskID)
}

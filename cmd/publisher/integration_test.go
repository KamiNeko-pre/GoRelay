package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	taskmodel "gorelay/internal/task"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestPublisherCommandIntegration(t *testing.T) {
	dsn, addr := os.Getenv("PUBLISHER_TEST_MYSQL_DSN"), os.Getenv("PUBLISHER_TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("dedicated PUBLISHER_TEST_MYSQL_DSN and PUBLISHER_TEST_REDIS_ADDR are required")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := db.AutoMigrate(&taskmodel.CrawlTask{}, &taskmodel.TaskOutbox{}, &taskmodel.TaskEvent{}); err != nil {
		t.Fatal(err)
	}
	var initialCount int64
	if err := db.Model(&taskmodel.TaskOutbox{}).Count(&initialCount).Error; err != nil {
		t.Fatal(err)
	}
	if initialCount != 0 {
		t.Fatal("use an empty, dedicated test database")
	}
	stream := fmt.Sprintf("publisher-command-test:%d", time.Now().UnixNano())
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	defer client.Del(context.Background(), stream)
	now := time.Now()
	task := taskmodel.CrawlTask{RequestKey: stream, SourceID: "test", URL: "https://example.com", Status: taskmodel.StatusQueued, QueuedAt: &now}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Delete(&task)
	row := taskmodel.TaskOutbox{TaskID: task.ID}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Delete(&row)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "configs"), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"mysql":     map[string]any{"dsn": dsn, "max_idle_conns": 1, "max_open_conns": 2},
		"redis":     map[string]any{"addr": addr, "stream": stream},
		"publisher": map[string]any{"batch_size": 1, "poll_interval_ms": 20, "batch_timeout_seconds": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "configs", "config.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("publisher exit: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("publisher failed to stop after cancellation")
		}
	}()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatal("publisher did not deliver before the deadline")
		case <-ticker.C:
			var stored taskmodel.TaskOutbox
			if err := db.First(&stored, row.ID).Error; err != nil {
				t.Fatal(err)
			}
			if stored.PublishedAt == nil {
				continue
			}
			messages, err := client.XRange(ctx, stream, stored.MessageID, stored.MessageID).Result()
			if err != nil || len(messages) != 1 {
				t.Fatalf("messages=%d error=%v", len(messages), err)
			}
			if fmt.Sprint(messages[0].Values["task_id"]) != fmt.Sprint(task.ID) {
				t.Fatal("wrong task ID published")
			}
			t.Log("real command startup automatically published the pending task and marked Outbox; shutdown is checked on return")
			return
		}
	}
}

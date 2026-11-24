package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorelay/internal/repository"
	taskmodel "gorelay/internal/task"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestLoadCrawlTaskRejectsInvalidMessage(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		cause  error
	}{
		{name: "missing field"},
		{name: "nil value", values: map[string]any{"task_id": nil}},
		{name: "wrong type", values: map[string]any{"task_id": 123}},
		{name: "empty", values: map[string]any{"task_id": ""}, cause: strconv.ErrSyntax},
		{name: "text", values: map[string]any{"task_id": "abc"}, cause: strconv.ErrSyntax},
		{name: "negative", values: map[string]any{"task_id": "-1"}, cause: strconv.ErrSyntax},
		{name: "overflow", values: map[string]any{"task_id": "18446744073709551616"}, cause: strconv.ErrRange},
		{name: "zero", values: map[string]any{"task_id": "0"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			task, err := loadCrawlTaskFromMessage(context.Background(), redis.XMessage{ID: "test-message", Values: test.values})
			if err == nil || task != (taskmodel.CrawlTask{}) {
				t.Fatalf("task=%+v error=%v, want zero task and error", task, err)
			}
			if test.cause != nil && !errors.Is(err, test.cause) {
				t.Fatalf("error=%v, want cause %v preserved", err, test.cause)
			}
		})
	}
}

func TestLoadCrawlTaskFromMessageIntegration(t *testing.T) {
	dsn := os.Getenv("MESSAGE_TASK_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("MESSAGE_TASK_TEST_MYSQL_DSN is required")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&taskmodel.CrawlTask{}); err != nil {
		t.Fatal(err)
	}
	previous := repository.DB
	repository.DB = db
	t.Cleanup(func() { repository.DB = previous })
	task := taskmodel.CrawlTask{RequestKey: fmt.Sprintf("message-task-test-%d", time.Now().UnixNano()), SourceID: "test", URL: "https://example.com/item", Status: taskmodel.StatusQueued}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Delete(&task).Error; err != nil {
			t.Error(err)
		}
	})
	message := redis.XMessage{ID: "not-a-mysql-task-id", Values: map[string]any{"task_id": strconv.FormatUint(task.ID, 10)}}
	loaded, err := loadCrawlTaskFromMessage(context.Background(), message)
	if err != nil || loaded.ID != task.ID || loaded.URL != task.URL || loaded.Status != task.Status {
		t.Fatalf("loaded=%+v error=%v", loaded, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loadCrawlTaskFromMessage(ctx, message); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation cause lost: %v", err)
	}
	if err := db.Delete(&task).Error; err != nil {
		t.Fatal(err)
	}
	loaded, err = loadCrawlTaskFromMessage(context.Background(), message)
	if loaded != (taskmodel.CrawlTask{}) || !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("missing task: task=%+v error=%v", loaded, err)
	}
}

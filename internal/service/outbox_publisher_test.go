package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorelay/internal/config"
	"gorelay/internal/queue"
	"gorelay/internal/repository"
	taskmodel "gorelay/internal/task"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestPublisherInvalidBatchLimit(t *testing.T) {
	for _, limit := range []int{0, -1} {
		count, err := PublishPendingTaskOutboxes(context.Background(), limit)
		if count != 0 || !errors.Is(err, repository.ErrInvalidOutboxBatchSize) {
			t.Fatalf("limit=%d: count=%d error=%v", limit, count, err)
		}
	}
}

// Run only against a dedicated test database: the publisher scans all Outbox rows.
func TestOutboxPublisherIntegration(t *testing.T) {
	dsn, addr := os.Getenv("OUTBOX_TEST_MYSQL_DSN"), os.Getenv("OUTBOX_TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("dedicated OUTBOX_TEST_MYSQL_DSN and OUTBOX_TEST_REDIS_ADDR are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
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
	previousDB, previousClient := repository.DB, queue.Client
	repository.DB = db
	t.Cleanup(func() { repository.DB, queue.Client = previousDB, previousClient })
	client := redis.NewClient(&redis.Options{Addr: addr})
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	queue.Client = client
	stream := fmt.Sprintf("outbox-test:%d", time.Now().UnixNano())
	configDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(configDir, "configs"), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"redis": map[string]any{"addr": addr, "stream": stream}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "configs", "config.yaml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(configDir)
	config.InitConfig()
	t.Cleanup(func() { _ = client.Del(context.Background(), stream).Err() })
	var taskIDs []uint64
	t.Cleanup(func() {
		if len(taskIDs) == 0 {
			return
		}
		if err := db.Where("task_id IN ?", taskIDs).Delete(&taskmodel.TaskOutbox{}).Error; err != nil {
			t.Error(err)
		}
		if err := db.Where("id IN ?", taskIDs).Delete(&taskmodel.CrawlTask{}).Error; err != nil {
			t.Error(err)
		}
	})
	create := func() uint64 {
		t.Helper()
		task := taskmodel.CrawlTask{RequestKey: fmt.Sprintf("%s:%d", stream, len(taskIDs)), Target: "test", URL: "https://example.com"}
		if err := repository.CreateQueuedCrawlTaskWithOutbox(ctx, &task); err != nil {
			t.Fatal(err)
		}
		taskIDs = append(taskIDs, task.ID)
		return task.ID
	}
	loadOutbox := func(taskID uint64) taskmodel.TaskOutbox {
		t.Helper()
		var row taskmodel.TaskOutbox
		if err := db.Where("task_id = ?", taskID).First(&row).Error; err != nil {
			t.Fatal(err)
		}
		return row
	}
	for i := 0; i < 3; i++ {
		create()
	}
	for _, want := range []int{2, 1, 0} {
		count, err := PublishPendingTaskOutboxes(ctx, 2)
		if err != nil || count != want {
			t.Fatalf("count=%d error=%v, want %d", count, err, want)
		}
	}
	entries, err := client.XRange(ctx, stream, "-", "+").Result()
	if err != nil || len(entries) != 3 {
		t.Fatalf("entries=%d error=%v", len(entries), err)
	}
	for i, entry := range entries {
		row := loadOutbox(taskIDs[i])
		if len(entry.Values) != 1 || fmt.Sprint(entry.Values["task_id"]) != fmt.Sprint(taskIDs[i]) || row.MessageID != entry.ID || row.PublishedAt == nil {
			t.Fatalf("mismatched message/outbox: %+v %+v", entry, row)
		}
	}
	t.Log("batch counts 2/1/0; Redis contains only task_id; message IDs match MySQL")

	missingRedisTask := create()
	queue.Client = nil
	count, err := PublishPendingTaskOutboxes(ctx, 2)
	queue.Client = client
	if err == nil || count != 0 || loadOutbox(missingRedisTask).PublishedAt != nil {
		t.Fatalf("failed publication was marked: count=%d err=%v", count, err)
	}
	if count, err := PublishPendingTaskOutboxes(ctx, 2); err != nil || count != 1 {
		t.Fatalf("retry: count=%d err=%v", count, err)
	}
	t.Log("unavailable Redis client leaves Outbox unpublished; retry completes")

	writebackTask := create()
	injected := errors.New("injected Outbox update failure")
	callback := "test:outbox_writeback_failure"
	if err := db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "task_outboxes" {
			tx.AddError(injected)
		}
	}); err != nil {
		t.Fatal(err)
	}
	count, err = PublishPendingTaskOutboxes(ctx, 2)
	if removeErr := db.Callback().Update().Remove(callback); removeErr != nil {
		t.Fatal(removeErr)
	}
	if count != 0 || !errors.Is(err, injected) || loadOutbox(writebackTask).PublishedAt != nil {
		t.Fatalf("writeback failure: count=%d err=%v", count, err)
	}
	if count, err := PublishPendingTaskOutboxes(ctx, 2); err != nil || count != 1 {
		t.Fatalf("writeback retry: count=%d err=%v", count, err)
	}
	entries, err = client.XRange(ctx, stream, "-", "+").Result()
	if err != nil {
		t.Fatal(err)
	}
	duplicates := 0
	for _, entry := range entries {
		if fmt.Sprint(entry.Values["task_id"]) == fmt.Sprint(writebackTask) {
			duplicates++
		}
	}
	if duplicates != 2 {
		t.Fatalf("want two deliveries after writeback failure, got %d", duplicates)
	}
	t.Log("XADD success + MySQL writeback failure returns count 0; retry produces two deliveries of the same task_id")
}

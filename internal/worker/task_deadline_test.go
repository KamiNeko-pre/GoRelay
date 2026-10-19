package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"gorm.io/gorm"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
)

func TestGeneratedTaskKeepsItsPersistedExecutionDeadline(t *testing.T) {
	addr := os.Getenv("GORELAY_LIVE_REDIS_ADDR")
	if addr == "" {
		t.Skip("real Redis required")
	}
	db := testdb.Open(t)
	if err := db.AutoMigrate(&task.CrawlTask{}, &task.TaskEvent{}); err != nil {
		t.Fatal(err)
	}
	previous := repository.DB
	repository.DB = db
	defer func() { repository.DB = previous }()
	client := redis.NewClient(&redis.Options{Addr: addr})
	defer client.Close()
	stream := fmt.Sprintf("task-deadline-%d", time.Now().UnixNano())
	group := "deadline-test"
	if err := client.XGroupCreateMkStream(context.Background(), stream, group, "0").Err(); err != nil {
		t.Fatal(err)
	}
	defer client.Del(context.Background(), stream)
	for _, tc := range []struct {
		name      string
		remaining time.Duration
		generated bool
	}{{"long-normal-task", time.Minute, true}, {"near-task-deadline", time.Second, true}, {"legacy", time.Minute, false}} {
		t.Run(tc.name, func(t *testing.T) {
			deadline := time.Now().Add(tc.remaining)
			value := map[string]any{"request_key": tc.name, "target": "test", "url": "https://example.com", "status": "queued", "execution_deadline": deadline}
			if tc.generated {
				value["program_hash"] = "verified-code"
			}
			raw, _ := json.Marshal(value)
			var row task.CrawlTask
			json.Unmarshal(raw, &row)
			if err := db.Create(&row).Error; err != nil {
				t.Fatal(err)
			}
			cfg := DefaultConfig(stream, group, "worker")
			cfg.ExecutionTimeout = time.Nanosecond
			var actual time.Time
			p, err := NewProcessor(client, cfg, func(ctx context.Context, _ task.CrawlTask) (repository.PersistTaskResult, error) {
				actual, _ = ctx.Deadline()
				return func(*gorm.DB) error { return nil }, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.ProcessNewMessage(context.Background(), redis.XMessage{ID: "0-1", Values: map[string]any{"task_id": fmt.Sprint(row.ID)}}); err != nil {
				t.Fatal(err)
			}
			if tc.generated && actual.Sub(deadline).Abs() > time.Millisecond {
				t.Fatal("worker replaced task deadline with legacy timeout", actual, deadline)
			}
			if !tc.generated && (time.Until(actual) < 3*time.Second || time.Until(actual) > 6*time.Second) {
				t.Fatal("legacy timeout changed", actual)
			}
		})
	}
}

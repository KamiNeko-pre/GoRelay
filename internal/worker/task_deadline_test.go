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
		reclaimed bool
	}{{"long-normal-task", time.Minute, true, false}, {"near-task-deadline", time.Second, true, false}, {"legacy", time.Minute, false, false}, {"expired-before-queue", -time.Second, true, false}, {"expired-after-reclaim", -time.Hour, true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			deadline := time.Now().Add(tc.remaining)
			value := map[string]any{"request_key": tc.name, "target": "test", "url": "https://example.com", "status": "queued", "execution_deadline": deadline}
			if tc.generated {
				value["program_hash"] = "verified-code"
			}
			if tc.reclaimed {
				value["status"] = "running"
				value["lease_until"] = time.Now().Add(-time.Minute)
				value["run_token"] = "expired-owner"
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
			message := redis.XMessage{ID: "0-1", Values: map[string]any{"task_id": fmt.Sprint(row.ID)}}
			if tc.reclaimed {
				err = p.ProcessClaimedMessage(context.Background(), message)
			} else {
				err = p.ProcessNewMessage(context.Background(), message)
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.generated && tc.remaining <= 0 {
				var saved task.CrawlTask
				if err := db.First(&saved, row.ID).Error; err != nil {
					t.Fatal(err)
				}
				if !actual.IsZero() || saved.Status != task.StatusFailed || saved.FailureCode != "lua_timeout" || saved.LeaseUntil != nil {
					t.Fatal("expired work did not close without execution", actual, saved.Status, saved.FailureCode, saved.LeaseUntil)
				}
				return
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

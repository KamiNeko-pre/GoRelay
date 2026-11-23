package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"gorelay/internal/repository"
	taskmodel "gorelay/internal/task"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestRedisClosureIntegration(t *testing.T) {
	dsn, addr := os.Getenv("CLOSURE_TEST_MYSQL_DSN"), os.Getenv("CLOSURE_TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("CLOSURE_TEST_MYSQL_DSN and CLOSURE_TEST_REDIS_ADDR required")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	previous := repository.DB
	repository.DB = db
	defer func() { repository.DB = previous }()
	if err := db.AutoMigrate(&taskmodel.CrawlTask{}, &repository.CrawlResult{}, &taskmodel.TaskEvent{}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	client := redis.NewClient(&redis.Options{Addr: addr, ContextTimeoutEnabled: true})
	defer client.Close()
	newFixture := func(t *testing.T) (*Processor, taskmodel.CrawlTask, redis.XMessage) {
		t.Helper()
		key := fmt.Sprintf("closure-%d", time.Now().UnixNano())
		cfg := DefaultConfig(key, "group", "worker")
		cfg.ClaimMinIdle = 20 * time.Millisecond
		cfg.ClaimInterval = 20 * time.Millisecond
		cfg.ReadBlock = 100 * time.Millisecond
		p, err := NewProcessor(client, cfg)
		if err != nil {
			t.Fatal(err)
		}
		task := taskmodel.CrawlTask{RequestKey: key, Target: "test", URL: "https://example.com", Status: taskmodel.StatusQueued}
		if err := db.Create(&task).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			client.Del(ctx, key)
			db.Delete(&repository.CrawlResult{}, "task_id = ?", task.ID)
			db.Delete(&task)
		})
		if err := client.XGroupCreateMkStream(ctx, key, "group", "0").Err(); err != nil {
			t.Fatal(err)
		}
		id, err := client.XAdd(ctx, &redis.XAddArgs{Stream: key, Values: map[string]any{"task_id": task.ID}}).Result()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{Group: "group", Consumer: "worker", Streams: []string{key, ">"}, Count: 1}).Result(); err != nil {
			t.Fatal(err)
		}
		return p, task, redis.XMessage{ID: id, Values: map[string]any{"task_id": strconv.FormatUint(task.ID, 10)}}
	}
	save := func(task taskmodel.CrawlTask) repository.PersistTaskResult {
		return func(tx *gorm.DB) error {
			return tx.Create(&repository.CrawlResult{TaskID: task.ID, Payload: `{"ok":true}`}).Error
		}
	}
	assertResult := func(t *testing.T, p *Processor, task taskmodel.CrawlTask, want taskmodel.Status, count int64, pending int64) {
		t.Helper()
		var stored taskmodel.CrawlTask
		if err := db.First(&stored, task.ID).Error; err != nil {
			t.Fatal(err)
		}
		var n int64
		db.Model(&repository.CrawlResult{}).Where("task_id = ?", task.ID).Count(&n)
		pel, err := client.XPending(ctx, p.config.Stream, p.config.Group).Result()
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != want || n != count || pel.Count != pending {
			t.Fatalf("status=%s rows=%d pending=%d; want %s/%d/%d", stored.Status, n, pel.Count, want, count, pending)
		}
	}
	t.Run("success duplicate and terminal ACK", func(t *testing.T) {
		p, task, msg := newFixture(t)
		var executions atomic.Int64
		p.execute = func(ctx context.Context, task taskmodel.CrawlTask) (repository.PersistTaskResult, error) {
			executions.Add(1)
			return save(task), nil
		}
		if err := p.ProcessNewMessage(ctx, msg); err != nil {
			t.Fatal(err)
		}
		if err := p.ProcessClaimedMessage(ctx, msg); err != nil {
			t.Fatal(err)
		}
		assertResult(t, p, task, taskmodel.StatusSucceeded, 1, 0)
		if executions.Load() != 1 {
			t.Fatal("duplicate execution")
		}
	})
	t.Run("rollback leaves PEL then actual reclaimer recovers", func(t *testing.T) {
		p, task, msg := newFixture(t)
		injected := errors.New("injected persistence failure")
		p.execute = func(ctx context.Context, task taskmodel.CrawlTask) (repository.PersistTaskResult, error) {
			return func(tx *gorm.DB) error {
				if err := save(task)(tx); err != nil {
					return err
				}
				return injected
			}, nil
		}
		if err := p.ProcessNewMessage(ctx, msg); !errors.Is(err, injected) {
			t.Fatalf("error=%v", err)
		}
		assertResult(t, p, task, taskmodel.StatusRunning, 0, 1)
		db.Model(&taskmodel.CrawlTask{}).Where("id = ?", task.ID).Update("lease_until", time.Now().Add(-time.Second))
		p.execute = func(ctx context.Context, task taskmodel.CrawlTask) (repository.PersistTaskResult, error) {
			return save(task), nil
		}
		r, _ := NewReclaimer(client, p.config, p)
		runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- r.Run(runCtx) }()
		for runCtx.Err() == nil {
			var n int64
			db.Model(&repository.CrawlResult{}).Where("task_id = ?", task.ID).Count(&n)
			if n == 1 {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(30 * time.Millisecond)
		cancel()
		<-done
		assertResult(t, p, task, taskmodel.StatusSucceeded, 1, 0)
	})
	t.Run("old owner fenced after expired lease replacement", func(t *testing.T) {
		p, task, _ := newFixture(t)
		old, err := repository.ClaimCrawlTask(ctx, task, time.Minute, false)
		if err != nil {
			t.Fatal(err)
		}
		db.Model(&taskmodel.CrawlTask{}).Where("id = ?", task.ID).Update("lease_until", time.Now().Add(-time.Second))
		var running taskmodel.CrawlTask
		db.First(&running, task.ID)
		fresh, err := repository.ClaimCrawlTask(ctx, running, time.Minute, true)
		if err != nil {
			t.Fatal(err)
		}
		err = repository.CompleteCrawlTask(ctx, task.ID, taskmodel.StatusSucceeded, "", "", time.Now(), old, save(task))
		if !errors.Is(err, repository.ErrTaskTransitionConflict) {
			t.Fatalf("stale owner error=%v", err)
		}
		assertResult(t, p, task, taskmodel.StatusRunning, 0, 1)
		if err := repository.CompleteCrawlTask(ctx, task.ID, taskmodel.StatusSucceeded, "", "", time.Now(), fresh, save(task)); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("concurrent messages use one active execution", func(t *testing.T) {
		p, task, msg := newFixture(t)
		var executions atomic.Int64
		entered := make(chan struct{})
		release := make(chan struct{})
		p.execute = func(ctx context.Context, task taskmodel.CrawlTask) (repository.PersistTaskResult, error) {
			executions.Add(1)
			close(entered)
			<-release
			return save(task), nil
		}
		done := make(chan error, 1)
		go func() { done <- p.ProcessNewMessage(ctx, msg) }()
		<-entered
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				other, _ := NewProcessor(client, p.config)
				other.execute = p.execute
				if err := other.ProcessClaimedMessage(ctx, msg); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if executions.Load() != 1 {
			t.Fatal("active lease was stolen")
		}
		assertResult(t, p, task, taskmodel.StatusSucceeded, 1, 0)
	})
	t.Run("panic becomes failed without result", func(t *testing.T) {
		p, task, msg := newFixture(t)
		p.execute = func(context.Context, taskmodel.CrawlTask) (repository.PersistTaskResult, error) { panic("test panic") }
		if err := p.ProcessNewMessage(ctx, msg); err != nil {
			t.Fatal(err)
		}
		assertResult(t, p, task, taskmodel.StatusFailed, 0, 0)
	})
	t.Run("commit survives ACK failure and redelivery only ACKs", func(t *testing.T) {
		p, task, msg := newFixture(t)
		closed := redis.NewClient(&redis.Options{Addr: addr})
		closed.Close()
		p.client = closed
		p.execute = func(ctx context.Context, task taskmodel.CrawlTask) (repository.PersistTaskResult, error) {
			return save(task), nil
		}
		if err := p.ProcessNewMessage(ctx, msg); !errors.Is(err, redis.ErrClosed) {
			t.Fatalf("ACK error=%v", err)
		}
		assertResult(t, p, task, taskmodel.StatusSucceeded, 1, 1)
		p.client = client
		p.execute = func(context.Context, taskmodel.CrawlTask) (repository.PersistTaskResult, error) {
			t.Fatal("terminal task executed")
			return nil, nil
		}
		if err := p.ProcessClaimedMessage(ctx, msg); err != nil {
			t.Fatal(err)
		}
		assertResult(t, p, task, taskmodel.StatusSucceeded, 1, 0)
	})
}

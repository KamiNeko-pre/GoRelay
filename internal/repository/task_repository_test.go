package repository

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	taskmodel "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
)

func TestTransitionCrawlTaskRejectsInvalidTransitionBeforeDatabase(t *testing.T) {
	err := TransitionCrawlTask(
		context.Background(),
		1,
		taskmodel.StatusSucceeded,
		taskmodel.StatusRunning,
		nil,
	)

	if !errors.Is(err, ErrInvalidTaskTransition) {
		t.Fatalf("expected ErrInvalidTaskTransition, got %v", err)
	}
}

func TestTransitionCrawlTaskCompareAndSet(t *testing.T) {
	db := testdb.Open(t)
	if err := db.AutoMigrate(&taskmodel.CrawlTask{}); err != nil {
		t.Fatalf("migrate crawl tasks: %v", err)
	}

	previousDB := DB
	DB = db
	t.Cleanup(func() { DB = previousDB })

	requestKey := fmt.Sprintf("integration-cas-%d", time.Now().UnixNano())
	crawlTask := taskmodel.CrawlTask{
		RequestKey: requestKey,
		Target:     "integration_test",
		URL:        "https://example.com/tasks",
		Status:     taskmodel.StatusQueued,
	}
	if err := CreateCrawlTask(context.Background(), &crawlTask); err != nil {
		t.Fatalf("create crawl task: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Where("request_key = ?", requestKey).Delete(&taskmodel.CrawlTask{}).Error; err != nil {
			t.Errorf("clean up crawl task: %v", err)
		}
	})

	if err := TransitionCrawlTask(context.Background(), crawlTask.ID, taskmodel.StatusQueued, taskmodel.StatusRunning, nil); err != nil {
		t.Fatalf("first transition should succeed: %v", err)
	}
	if err := TransitionCrawlTask(context.Background(), crawlTask.ID, taskmodel.StatusQueued, taskmodel.StatusRunning, nil); !errors.Is(err, ErrTaskTransitionConflict) {
		t.Fatalf("second transition should conflict, got %v", err)
	}

	storedTask, err := GetCrawlTask(context.Background(), crawlTask.ID)
	if err != nil {
		t.Fatalf("load crawl task: %v", err)
	}
	if storedTask.Status != taskmodel.StatusRunning {
		t.Fatalf("expected status %q, got %q", taskmodel.StatusRunning, storedTask.Status)
	}
}

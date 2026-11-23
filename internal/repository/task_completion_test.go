package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	taskmodel "gorelay/internal/task"
	"gorm.io/gorm"
)

func TestCompleteCrawlTaskRejectsInvalidArguments(t *testing.T) {
	previousDB := DB
	DB = nil
	t.Cleanup(func() { DB = previousDB })

	callback := PersistTaskResult(func(tx *gorm.DB) error {
		t.Fatal("invalid arguments must not invoke the persistence callback")
		return nil
	})
	cases := []struct {
		name    string
		id      uint64
		to      taskmodel.Status
		persist PersistTaskResult
		cause   error
	}{
		{"zero ID", 0, taskmodel.StatusSucceeded, callback, nil},
		{"pending", 1, taskmodel.StatusPending, callback, ErrInvalidTaskTransition},
		{"queued", 1, taskmodel.StatusQueued, callback, ErrInvalidTaskTransition},
		{"running", 1, taskmodel.StatusRunning, callback, ErrInvalidTaskTransition},
		{"unknown status", 1, taskmodel.Status("unknown"), callback, ErrInvalidTaskTransition},
		{"success without callback", 1, taskmodel.StatusSucceeded, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CompleteCrawlTask(context.Background(), tc.id, tc.to, "", "", time.Now(), "test-token", tc.persist)
			if err == nil {
				t.Fatal("expected rejection before accessing the database")
			}
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Fatalf("error=%v, want cause %v", err, tc.cause)
			}
		})
	}
}

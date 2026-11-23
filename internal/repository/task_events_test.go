package repository

import (
	"context"
	"testing"

	taskmodel "gorelay/internal/task"
)

func TestRecordTaskEventValidatesInputBeforeDatabase(t *testing.T) {
	previousDB := DB
	DB = nil
	t.Cleanup(func() { DB = previousDB })

	if err := RecordTaskEvent(context.Background(), nil); err == nil {
		t.Fatal("expected nil event error")
	}
	if err := RecordTaskEvent(context.Background(), &taskmodel.TaskEvent{Stage: taskmodel.EventStageClaimed, Level: taskmodel.EventLevelInfo}); err == nil {
		t.Fatal("expected zero task ID error")
	}
}

func TestListTaskEventsValidatesTaskIDBeforeDatabase(t *testing.T) {
	previousDB := DB
	DB = nil
	t.Cleanup(func() { DB = previousDB })

	if _, err := ListTaskEvents(context.Background(), 0); err == nil {
		t.Fatal("expected zero task ID error")
	}
}

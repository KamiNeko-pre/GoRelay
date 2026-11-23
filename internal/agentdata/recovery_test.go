package agentdata

import (
	"context"
	"fmt"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestQueueRestoreOnlyRequeuesLiveDurableFacts(t *testing.T) {
	s := NewStore(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	for n, state := range []string{"planning", "cancelled", "completed", "expired", "missing", "legacy"} {
		id := fmt.Sprint(n)
		if state != "legacy" && state != "missing" {
			r := Run{ID: id, RequestKey: id, Status: state, Deadline: now.Add(time.Hour), Cancelled: state == "cancelled"}
			if state == "expired" {
				r.Deadline = now.Add(-time.Hour)
			}
			s.DB.Create(&r)
		}
		if state == "legacy" {
			id = ""
		}
		row := task.CrawlTask{RunID: id, RequestKey: "task" + fmt.Sprint(n), Status: task.StatusQueued}
		s.DB.Create(&row)
		s.DB.Create(&task.TaskOutbox{TaskID: row.ID, PublishedAt: &now, MessageID: "old"})
	}
	n, err := s.ReconcileQueue(ctx, now)
	if err != nil || n != 2 {
		t.Fatalf("restored cancelled/terminal/dangling tasks or lost live ones: %d %v", n, err)
	}
	n, err = s.ReconcileQueue(ctx, now)
	if err != nil || n != 0 {
		t.Fatal("reconcile not idempotent")
	}
}

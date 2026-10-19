package knowledge

import (
	"context"
	"encoding/json"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestIndexCostBindsToOriginatingTaskNotMutableHead(t *testing.T) {
	db := testdb.Open(t)
	s := NewStore(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	db.AutoMigrate(&task.CrawlTask{})
	child := task.CrawlTask{RequestKey: "first", RunID: "run-first"}
	db.Create(&child)
	doc, _ := NewDocument(Input{Source: "fixture", SourceKey: "row", Title: "A", Body: "body", Format: "text", TaskID: child.ID})
	head, err := s.Put(context.Background(), doc, "fp")
	if err != nil {
		t.Fatal(err)
	}
	work, err := s.Claim(context.Background(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := s.startIndexAttempt(context.Background(), work)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	raw, _ := json.Marshal(attempt)
	_ = json.Unmarshal(raw, &wire)
	if wire["run_id"] != "run-first" || wire["task_id"] != float64(child.ID) {
		t.Fatalf("index work has no durable origin: %+v", wire)
	}
	db.Model(&Head{}).Where("id = ?", head.ID).Update("task_id", child.ID+1)
	rows, err := s.IndexAttemptsForRun(context.Background(), "run-first")
	if err != nil || len(rows) != 1 {
		t.Fatal("mutable head moved old cost to another run")
	}
}

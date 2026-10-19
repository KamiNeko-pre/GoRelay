package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/repository"
	task "go-lua-crawler/internal/task"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestExplicitCorrectionRetainsAuditButUnblocksDelivery(t *testing.T) {
	for _, field := range []string{"version", "license"} {
		t.Run(field, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			spec := TaskSpec{Goal: "correct public facts", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}, {Name: field, Type: "string"}}, JoinKeys: []string{"name"}}
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: "correction", Goal: spec.Goal, Spec: &spec})
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			seed := func(key, name, value string) (uint64, string) {
				child := task.CrawlTask{RunID: r.ID, RequestKey: key, Status: task.StatusSucceeded, URL: "https://fixture.example/"}
				if err := s.Data.DB.Create(&child).Error; err != nil {
					t.Fatal(err)
				}
				op := agentdata.Operation{RunID: r.ID, Key: key, Kind: "fixture", Stage: "succeeded"}
				s.Data.DB.Create(&op)
				s.Data.DB.Create(&agentdata.RunTask{RunID: r.ID, TaskID: child.ID, OperationID: op.ID})
				record := Record{Key: key, SchemaVersion: "v1", Fields: map[string]any{"name": name, field: value}, Provenance: Provenance{SourceID: key, URL: child.URL, FetchedAt: time.Now(), DataVersion: key, TaskID: child.ID}}
				raw, _ := json.Marshal(AcquisitionResult{Records: []Record{record}})
				s.Data.DB.Create(&repository.CrawlResult{TaskID: child.ID, Payload: string(raw)})
				return child.ID, key
			}
			old, oldKey := seed("old", "Alpha", "wrong")
			fresh, freshKey := seed("new", "Alpha", "correct")
			before, err := s.Evaluate(ctx, r)
			if err != nil || before.Coverage.Satisfied || len(before.Conflicts) == 0 {
				t.Fatal("conflict reproduction invalid")
			}
			args, _ := json.Marshal(map[string]any{"old_task_id": old, "old_record_key": oldKey, "new_task_id": fresh, "new_record_key": freshKey, "reason": "rechecked source; earlier extraction selected the wrong column"})
			call := ToolCall{ID: "replace", Type: "function", Function: FunctionCall{Name: "replace_record", Arguments: string(args)}}
			if _, err = s.executeDurableTool(ctx, r, call); err != nil {
				t.Fatal(err)
			}
			if _, err = s.executeDurableTool(ctx, r, call); err != nil {
				t.Fatal("receipt replay failed", err)
			}
			after, err := NewService(s.Data.DB).Evaluate(ctx, r)
			if err != nil || !after.Coverage.Satisfied || len(after.Records) != 1 || fmt.Sprint(after.Records[0].Fields[field]) != "correct" {
				t.Fatalf("old error still blocks corrected delivery: %+v %v", after, err)
			}
			audit, _ := s.ChildResults(ctx, r.ID)
			if len(audit) != 2 || len(audit[0].Records) != 1 {
				t.Fatal("original evidence was erased")
			}
			seed("other-source", "Alpha", "disagrees")
			result, _ := s.Evaluate(ctx, r)
			if result.Coverage.Satisfied || len(result.Conflicts) == 0 {
				t.Fatal("unselected cross-source conflict was hidden")
			}
			alien, alienKey := seed("other-entity", "Beta", "correct")
			badArgs, _ := json.Marshal(map[string]any{"old_task_id": fresh, "old_record_key": freshKey, "new_task_id": alien, "new_record_key": alienKey, "reason": "different entity"})
			call.ID = "bad"
			call.Function.Arguments = string(badArgs)
			if _, err = s.executeDurableTool(ctx, r, call); err == nil {
				t.Fatal("different business identity can replace a record")
			}
		})
	}
}

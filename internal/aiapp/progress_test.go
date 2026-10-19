package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"strings"
	"testing"
	"time"
)

func TestContextRestoresDurableProgressAfterHistoryRemoval(t *testing.T) {
	for _, goal := range []string{"historical evidence", "compare component versions"} {
		progress := `{"version":1,"requirements":[{"id":"scope","status":"missing","explanation":"EARLY_PERIOD_STILL_MISSING","citations":[]}],"notes":["RECENT_PERIOD_ALREADY_READ"],"next_purpose":"find early period"}`
		raw, _ := json.Marshal(map[string]any{"goal": goal, "progress_json": progress})
		var run agentdata.Run
		if err := json.Unmarshal(raw, &run); err != nil {
			t.Fatal(err)
		}
		state := LoopState{Run: run, Spec: &TaskSpec{Goal: goal, Requirements: []TaskRequirement{{ID: "scope", Description: "cover requested period"}}}}
		messages := []Message{{Role: "tool", Content: strings.Repeat("old evidence", 7000)}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "pending", Function: FunctionCall{Name: "read_evidence"}}}}}
		got, err := CompactContext(state, messages)
		if err != nil {
			t.Fatal(err)
		}
		packed, _ := json.Marshal(got)
		if !strings.Contains(string(packed), "EARLY_PERIOD_STILL_MISSING") || !strings.Contains(string(packed), "RECENT_PERIOD_ALREADY_READ") {
			t.Fatalf("durable progress lost after compaction: %s", packed)
		}
	}
}

func TestProgressCheckpointPersistsAndRejectsStaleVersionAndForeignRequirements(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := TaskSpec{Goal: "compare public components", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}}, Requirements: []TaskRequirement{{ID: "scope", Description: "compare requested components"}}}
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "progress", Goal: spec.Goal, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"expected_version": 0, "requirements": []any{map[string]any{"id": "scope", "status": "missing", "explanation": "one component pending", "citations": []string{}}}, "notes": []string{"first component inspected"}, "next_purpose": "collect remaining component"}
	call := func(id string) ToolCall {
		raw, _ := json.Marshal(args)
		return ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: "update_task_progress", Arguments: string(raw)}}
	}
	firstCall := call("checkpoint")
	if _, err = s.executeDurableTool(ctx, r, firstCall); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Data.Run(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	packed, _ := json.Marshal(stored)
	if !strings.Contains(string(packed), "first component inspected") {
		t.Fatal("checkpoint missing from durable run")
	}
	if _, err = s.executeDurableTool(ctx, stored, call("stale")); err == nil {
		t.Fatal("stale checkpoint overwrote current progress")
	}
	args["expected_version"] = 1
	args["requirements"] = []any{map[string]any{"id": "invented", "status": "missing", "explanation": "new requirement", "citations": []string{}}}
	if _, err = s.executeDurableTool(ctx, stored, call("foreign")); err == nil {
		t.Fatal("unfrozen requirement accepted")
	}
	// Replayed committed operation is read, without creating another checkpoint.
	if _, err = s.executeDurableTool(ctx, stored, firstCall); err != nil {
		t.Fatalf("committed checkpoint could not replay: %v", err)
	}
}

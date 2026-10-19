package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestToolPlanFreezesRequirementsAndScope(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "freeze", Goal: "release list", SourceScope: []string{"example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	spec := TaskSpec{Goal: r.Goal, SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}}
	data, _ := json.Marshal(spec)
	call := ToolCall{ID: "plan1", Type: "function", Function: FunctionCall{Name: "plan_task", Arguments: string(data)}}
	if _, err = s.executeDurableTool(ctx, r, call); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Data.Run(ctx, r.ID)
	var frozen TaskSpec
	json.Unmarshal([]byte(saved.SpecJSON), &frozen)
	if len(frozen.SourceScope) != 1 || frozen.SourceScope[0] != "example.com" {
		t.Fatal("model changed original network scope")
	}
	spec.RequiredFields = append(spec.RequiredFields, FieldRequirement{Name: "version", Type: "string"})
	data, _ = json.Marshal(spec)
	call.ID = "plan2"
	call.Function.Arguments = string(data)
	if _, err = s.executeDurableTool(ctx, r, call); err == nil {
		t.Fatal("requirements rewritten after plan freeze")
	}
}

func TestToolRepeatedObservationReusesStoredResult(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, _ := s.Create(ctx, CreateRunRequest{RequestKey: "reuse", Goal: "goal"})
	r, _ = s.Data.Claim(ctx, r.ID, time.Minute)
	call := ToolCall{ID: "list1", Type: "function", Function: FunctionCall{Name: "list_sources", Arguments: `{}`}}
	first, err := s.executeDurableTool(ctx, r, call)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Data.DB.Create(&agentdata.Trace{RunID: r.ID, Stage: "unrelated"}).Error; err != nil {
		t.Fatal(err)
	}
	second, err := s.executeDurableTool(ctx, r, call)
	if err != nil || string(first.Data) != string(second.Data) {
		t.Fatalf("replay: %v", err)
	}
	stored, _ := s.Data.Run(ctx, r.ID)
	var budget BudgetSnapshot
	json.Unmarshal([]byte(stored.BudgetJSON), &budget)
	if budget.Spent.ToolCalls != 1 {
		t.Fatalf("repeat charged twice: %+v", budget.Spent)
	}
	call.Function.Arguments = `{"extra":true}`
	if _, err = s.executeDurableTool(ctx, r, call); err == nil {
		t.Fatal("same call ID changed input")
	}
}

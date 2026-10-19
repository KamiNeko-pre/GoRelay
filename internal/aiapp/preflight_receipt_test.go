package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestRejectedToolArgumentsHaveDurableFeedbackWithoutExternalReservation(t *testing.T) {
	for _, fn := range []FunctionCall{
		{Name: "inspect_source", Arguments: `{}`},
		{Name: "validate_program", Arguments: `{"url":"https://example.com/","kind":"composition","code":"return true,{}"}`},
	} {
		t.Run(fn.Name, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			r, err := s.Create(ctx, CreateRunRequest{RequestKey: fn.Name, Goal: "collect"})
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			call := ToolCall{ID: "bad-arguments", Type: "function", Function: fn}
			out, err := s.executeDurableTool(ctx, r, call)
			if err == nil {
				t.Fatal("invalid proposal accepted")
			}
			cause := err.Error()
			ops, err := s.Data.Operations(ctx, r.ID)
			if err != nil || len(ops) != 1 || ops[0].Stage != "failed" {
				t.Fatalf("preflight rejection vanished from durable ledger: %+v %v", ops, err)
			}
			var saved ToolResult
			if err := json.Unmarshal([]byte(ops[0].ResultJSON), &saved); err != nil || saved.Error != cause || saved.CallID != call.ID || out.Error != cause {
				t.Fatal("exact corrective cause was not saved")
			}
			if _, err := s.executeDurableTool(ctx, r, call); err == nil {
				t.Fatal("saved rejection replay lost error")
			}
			current, _ := s.Data.Run(ctx, r.ID)
			var budget BudgetSnapshot
			json.Unmarshal([]byte(current.BudgetJSON), &budget)
			if budget.Spent.ToolCalls != 1 || budget.Spent.HTTPRequests != 0 {
				t.Fatal("rejected arguments reserved external work or replay was charged twice", budget.Spent)
			}
			messages := []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: ops[0].ResultJSON}}
			if !assessedHistoryRound(LoopState{Run: current, Budget: budget, Operations: ops}, messages) {
				t.Fatal("saved rejection prevents completed history from compacting")
			}
		})
	}
}

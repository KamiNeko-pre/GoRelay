package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"testing"
)

func TestCheckpointIsAdvisoryAndCanLeaveRequirementsMissing(t *testing.T) {
	s, r, search := memoryRun(t, "collect facts before evidence is sufficient")
	ctx := context.Background()
	for n := 0; n < 3; n++ {
		if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: string(rune('a' + n)), Kind: "model", Stage: "succeeded", ResultJSON: `{}`}).Error; err != nil {
			t.Fatal(err)
		}
	}
	call := ToolCall{ID: "continue", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"needed public facts"}`}}
	if _, err := s.executeDurableTool(ctx, r, call); err != nil || search.calls != 1 {
		t.Fatalf("checkpoint blocked needed acquisition: %v", err)
	}
	call = ToolCall{ID: "partial-checkpoint", Type: "function", Function: FunctionCall{Name: "update_task_progress", Arguments: `{"requirements":[],"notes":["still investigating"]}`}}
	if _, err := s.executeDurableTool(ctx, r, call); err != nil {
		t.Fatal("incomplete checkpoint rejected", err)
	}
	current, _ := s.Data.Run(ctx, r.ID)
	var p TaskProgress
	_ = json.Unmarshal([]byte(current.ProgressJSON), &p)
	if len(p.Requirements) != 1 || p.Requirements[0].Status != "missing" || len(p.Requirements[0].Citations) != 0 {
		t.Fatal("host did not preserve missing requirement", p)
	}
	for _, args := range []string{
		`{"requirements":[{"id":"foreign","status":"missing","explanation":"unknown","citations":[]}]}`,
		`{"requirements":[{"id":"scope","status":"satisfied","explanation":"claim without evidence","citations":[]}]}`,
		`{"requirements":[{"id":"scope","status":"missing","explanation":"unsupported reference","citations":["ev_foreign"]}]}`,
	} {
		call.ID += "x"
		call.Function.Arguments = args
		if _, err := s.executeDurableTool(ctx, current, call); err == nil {
			t.Fatal("checkpoint accepted fabricated requirement/evidence", args)
		}
	}
}

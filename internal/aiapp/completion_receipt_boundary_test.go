package aiapp

import (
	"context"
	"encoding/json"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

func TestCompletionUsesCanonicalReceiptNotBoundedModelView(t *testing.T) {
	s := decisionState()
	s.Run.ID = "owned-run"
	s.Spec = &TaskSpec{Goal: "deliver facts", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "body", Type: "string"}}}
	s.Result = &TaskResult{Coverage: CoverageReport{Satisfied: true, ValidRecords: 1}, SummaryRequired: true, Answer: &RAGAnswer{Status: "answered"}}
	full, _ := toolData(ToolCall{ID: "finish", Function: FunctionCall{Name: "finish_task"}}, map[string]any{"status": "completed", "reason": "cited complete delivery"})
	raw, _ := json.Marshal(full)
	s.Operations = []agentdata.Operation{{ID: 1, RunID: s.Run.ID, Key: "tool:finish", Kind: "finish_task", Stage: "succeeded", ResultJSON: string(raw)}}
	full.Data = json.RawMessage(`{"receipt_handle":"finish","observation":{"status":"completed"},"observation_truncated":true}`)
	preview, _ := json.Marshal(full)
	s.Messages = []Message{{Role: "tool", ToolCallID: "finish", Content: string(preview)}}
	action, err := RunTaskLoop(context.Background(), s)
	if err != nil || action.Kind != "finish" || action.Status != "completed" {
		t.Fatal("bounded feedback erased host-accepted completion", action, err)
	}
	s.Operations[0].RunID = "another-run"
	action, err = RunTaskLoop(context.Background(), s)
	if err != nil || action.Kind == "finish" {
		t.Fatal("foreign receipt completed current run", action, err)
	}
}

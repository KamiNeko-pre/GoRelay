package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"strings"
	"testing"
)

func TestContextDoesNotDuplicateFeedbackAlreadyInWorkingHistory(t *testing.T) {
	state := decisionState()
	state.Run.ID = "history-feedback"
	state.Budget.LoopVersion = 5
	state.Run.BudgetJSON = `{"loop_version":5}`
	state.Run.InputJSON = `{"goal":"collect"}`
	for _, id := range []string{"older", "current"} {
		call := ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: `{}`}}
		receipt := ToolResult{CallID: id, Name: "validate_program"}
		receipt.Data, _ = json.Marshal(map[string]any{"candidate_id": id + "-asset"})
		raw, _ := json.Marshal(receipt)
		state.Operations = append(state.Operations, agentdata.Operation{ID: uint64(len(state.Operations) + 1), RunID: state.Run.ID, Key: "tool:" + id, Kind: receipt.Name, InputHash: id, Stage: "succeeded", ResultJSON: string(raw)})
		if id == "current" {
			state.Messages = []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: id, Content: string(raw)}}
		}
	}
	messages, err := CompactContext(state, state.Messages)
	if err != nil {
		t.Fatal(err)
	}
	var host string
	for _, m := range messages {
		if m.ToolCallID == hostContextCallID(state) {
			host = m.Content
		}
	}
	if !strings.Contains(host, "older-asset") {
		t.Fatal("evicted asset has no recovery feedback")
	}
	if strings.Contains(host, "current-asset") {
		t.Fatal("current tool result duplicated into volatile memory")
	}
	for _, m := range messages {
		if m.ToolCallID == "current" && strings.Contains(m.Content, "current-asset") {
			return
		}
	}
	t.Fatal("current original tool reply was removed")
}

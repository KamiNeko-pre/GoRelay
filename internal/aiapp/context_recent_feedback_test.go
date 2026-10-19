package aiapp

import (
	"encoding/json"
	"strings"
	"testing"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
)

func TestV3ContextKeepsDistinctAcquiredPagesAfterCompaction(t *testing.T) {
	state := decisionState()
	state.Run.ID = "page-inventory"
	state.Budget.LoopVersion = 3
	var messages []Message
	for i := 0; i < 11; i++ {
		id := string(rune('a' + i))
		name := "get_task_results"
		data := map[string]any{"text": strings.Repeat("later material ", 400)}
		if i < 2 {
			name = "inspect_source"
			page := sourceInspection{Source: acquisition.Response{Body: `<main>public fact</main>`, Receipt: acquisition.Receipt{URL: "https://fixture.example/" + id, Status: 200, Method: "GET"}}}
			raw, _ := json.Marshal(page)
			_ = json.Unmarshal(raw, &data)
		}
		call := ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: `{}`}}
		result, _ := toolData(call, data)
		raw, _ := json.Marshal(result)
		state.Operations = append(state.Operations, agentdata.Operation{ID: uint64(i + 1), RunID: state.Run.ID, Key: "tool:" + id, Kind: name, Stage: "succeeded", ResultJSON: string(raw)})
		messages = append(messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}}, Message{Role: "tool", ToolCallID: id, Content: string(raw)})
	}
	foreign := state.Operations[0]
	foreign.RunID = "another-run"
	foreign.Key = "tool:foreign-page"
	state.Operations = append(state.Operations, foreign)
	state.Messages = messages
	got, err := CompactContext(state, messages)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		found := false
		for _, message := range got {
			if strings.Contains(message.Content, "https://fixture.example/"+id) {
				if message.Role != "tool" {
					t.Fatal("acquired metadata promoted into system")
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("acquired page %s lost when later tools displace inspection feedback", id)
		}
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "foreign-page") {
		t.Fatal("another Run entered page inventory")
	}
}

func TestV3ContextPreservesRecentFeedbackBeforeSizePressure(t *testing.T) {
	for _, name := range []string{"list_sources", "validate_program"} {
		t.Run(name, func(t *testing.T) {
			state := decisionState()
			state.Run.ID = "recent-feedback"
			state.Budget.LoopVersion = 3
			var messages []Message
			for i, tool := range []string{name, "discover_sources", "get_task_results"} {
				id := string(rune('a' + i))
				call := ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: tool, Arguments: `{}`}}
				data := map[string]any{"active": []any{}, "candidate_id": "candidate-checked", "artifact_hash": "version-checked"}
				result, _ := toolData(call, data)
				if tool == "discover_sources" {
					result.Error = "search transport did not execute native web search"
				}
				raw, _ := json.Marshal(result)
				state.Operations = append(state.Operations, agentdata.Operation{ID: uint64(i + 1), Key: "tool:" + id, Kind: tool, Stage: "succeeded", ResultJSON: string(raw)})
				messages = append(messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}}, Message{Role: "tool", ToolCallID: id, Content: string(raw)})
			}
			got, err := CompactContext(state, messages)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			for _, message := range got {
				if message.Role == "tool" {
					seen[message.ToolCallID] = true
				}
			}
			if !seen["a"] || !seen["b"] || !seen["c"] {
				t.Fatalf("recent source/candidate and failure feedback erased without a replacement: %+v", seen)
			}
			raw, _ := json.Marshal(got)
			if !strings.Contains(string(raw), "search transport did not execute native web search") || contextRequestBytes(state, got) > 32<<10 {
				t.Fatal("recent facts lost or fixture exceeds soft window")
			}
		})
	}
}

func TestV3ContextRetainsReceiptMeaningUnderWindowPressure(t *testing.T) {
	for _, name := range []string{"list_sources", "validate_program"} {
		t.Run(name, func(t *testing.T) {
			state := decisionState()
			state.Run.ID = "pressure-feedback"
			state.Budget.LoopVersion = 3
			var messages []Message
			for i := 0; i < 9; i++ {
				tool := "get_task_results"
				if i == 0 {
					tool = name
				}
				id := string(rune('a' + i))
				call := ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: tool, Arguments: `{}`}}
				data := map[string]any{"text": strings.Repeat("later receipt content ", 100)}
				if i == 0 {
					data = map[string]any{"active": []any{}, "candidate_id": "verified-candidate-under-pressure"}
				}
				result, _ := toolData(call, data)
				raw, _ := json.Marshal(result)
				state.Operations = append(state.Operations, agentdata.Operation{ID: uint64(i + 1), RunID: state.Run.ID, Key: "tool:" + id, Kind: tool, Stage: "succeeded", ResultJSON: string(raw)})
				messages = append(messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}}, Message{Role: "tool", ToolCallID: id, Content: string(raw)})
			}
			state.Messages = messages
			got, err := CompactContext(state, messages)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, message := range got {
				if strings.Contains(message.Content, "verified-candidate-under-pressure") {
					if message.Role != "tool" {
						t.Fatal("historical tool data promoted into system instructions")
					}
					found = true
				}
			}
			if !found || contextRequestBytes(state, got) > 64<<10 {
				t.Fatal("old feedback meaning erased instead of retained in a bounded data snapshot")
			}
		})
	}
}

func TestV3ReviewDoesNotDemandDisabledCheckpointTool(t *testing.T) {
	state := decisionState()
	state.Spec = &TaskSpec{Goal: "collect records", SchemaVersion: "v1"}
	state.Budget.LoopVersion = 3
	state.Budget.Spent.ModelCalls = 4
	for i := uint64(1); i <= 4; i++ {
		state.Operations = append(state.Operations, agentdata.Operation{ID: i, Kind: "model", Stage: "succeeded"})
	}
	if review := taskReview(state); review.Required || review.DeliveryRequired {
		t.Fatalf("retired model checkpoint ceremony still advertised: %+v", review)
	}
	state.Budget.LoopVersion = 2
	if !taskReview(state).Required {
		t.Fatal("legacy checkpoint protocol changed")
	}
	state.Budget.LoopVersion = 3
	state.Budget.Spent.ModelCalls = state.Budget.Limits.ModelCalls - 1
	if !taskReview(state).DeliveryRequired {
		t.Fatal("bounded delivery reserve was disabled")
	}
}

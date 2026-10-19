package aiapp

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"os"
	"strings"
	"testing"
)

func TestContextRemovesOldCompletedRoundsBeforeHardOverflow(t *testing.T) {
	for _, name := range []string{"inspect_source", "get_task_results"} {
		state := LoopState{Run: agentdata.Run{Goal: "preserve the original goal", ProgressJSON: `{"version":2,"notes":["durable evidence assessment"],"next_purpose":"finish remaining requirement"}`}}
		messages := []Message{}
		for n := 0; n < 4; n++ {
			id := fmt.Sprintf("round-%d", n)
			messages = append(messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: id, Function: FunctionCall{Name: name, Arguments: `{}`}}}})
			receipt, _ := toolData(ToolCall{ID: id, Function: FunctionCall{Name: name}}, map[string]string{"text": strings.Repeat("observed content ", 500)})
			raw, _ := json.Marshal(receipt)
			messages = append(messages, Message{Role: "tool", ToolCallID: id, Content: string(raw)})
			// Eviction is gated on the round being recorded and assessed in the
			// operation ledger (see assessedHistoryRound): a round the host never
			// durably replied to must stay visible. The fixture previously omitted
			// these rows, so compaction correctly refused to drop anything and the
			// request stayed over the ceiling this test exists to enforce.
			state.Operations = append(state.Operations, agentdata.Operation{
				ID: uint64(n + 1), Kind: name, Stage: "succeeded", Key: "tool:" + id, ResultJSON: string(raw),
			})
		}
		got, err := CompactContext(state, messages)
		if err != nil {
			t.Fatal(err)
		}
		// What this test actually protects: old completed rounds leave the
		// request before hard overflow, the newest round survives, and the
		// checkpoint is still carried. A byte ceiling is deliberately not
		// asserted: the fixed envelope (system prompt, frozen contract, host
		// snapshot) is not working history, and on a V3+ run it is large enough
		// that a single retained round can put the whole request above any
		// history-sized number. Asserting it would fail for a healthy run and
		// says nothing about eviction.
		if len(got) >= len(messages) {
			t.Fatalf("no round was evicted: kept %d of %d messages", len(got), len(messages))
		}
		var keptRounds []string
		for _, m := range got {
			if m.Role == "assistant" {
				for _, call := range m.ToolCalls {
					if call.Function.Name != "host_context" {
						keptRounds = append(keptRounds, call.ID)
					}
				}
			}
		}
		if len(keptRounds) != 1 || keptRounds[0] != "round-3" {
			t.Fatalf("eviction kept the wrong rounds: %v (want exactly round-3)", keptRounds)
		}
		raw, _ := json.Marshal(got)
		if !strings.Contains(string(raw), "durable evidence assessment") || !strings.Contains(string(raw), "round-3") {
			t.Fatal("checkpoint/latest round lost")
		}
	}
}

func TestLargeIndivisibleLatestRoundMayExceedSoftTarget(t *testing.T) {
	policy, err := configuredPromptPolicy()
	if err != nil {
		t.Fatal(err)
	}
	state := LoopState{Run: agentdata.Run{Goal: "keep a valid latest action"}, Budget: BudgetSnapshot{LoopVersion: 5, PromptPolicy: policy}}
	state.Run.BudgetJSON = string(mustJSON(state.Budget))
	call := ToolCall{ID: "pending", Function: FunctionCall{Name: "validate_program", Arguments: string(mustJSON(map[string]any{"code": strings.Repeat("local v=1\n", 3000)}))}}
	got, err := CompactContext(state, []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}})
	if err != nil || got[len(got)-1].ToolCalls[0].Function.Arguments != call.Function.Arguments {
		t.Fatal("soft target clipped an indivisible action", err)
	}
}

func TestContextCompactionKeepsPendingCallIDsAndFrozenSpec(t *testing.T) {
	s := LoopState{Run: agentdata.Run{Goal: "original goal"}, Spec: &TaskSpec{Goal: "original goal", SchemaVersion: "v1"}, Messages: []Message{{Role: "system", Content: AgentSystemPrompt}, {Role: "user", Content: "goal"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "old", Type: "function", Function: FunctionCall{Name: "inspect_source", Arguments: `{}`}}}}, {Role: "tool", ToolCallID: "old", Content: strings.Repeat("x", 70000)}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "pending", Type: "function", Function: FunctionCall{Name: "read_evidence", Arguments: `{"evidence_id":"ev_x"}`}}}}}}
	got, err := CompactContext(s, s.Messages)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if len(raw) > 64<<10 || got[len(got)-1].ToolCalls[0].ID != "pending" || !strings.Contains(string(raw), "original goal") {
		t.Fatal("lost pending identity or goal")
	}
}

func TestCompactionStoresFrozenRequirementsOnceForManyChildren(t *testing.T) {
	for _, count := range []int{5, 9} {
		spec := TaskSpec{Goal: strings.Repeat("original requirements ", 150), SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "body", Type: "string"}}}
		rawSpec, _ := json.Marshal(spec)
		state := LoopState{Run: agentdata.Run{Goal: spec.Goal}, Spec: &spec}
		policy, err := configuredPromptPolicy()
		if err != nil {
			t.Fatal(err)
		}
		state.Budget = BudgetSnapshot{LoopVersion: 5, PromptPolicy: policy}
		state.Run.BudgetJSON = string(mustJSON(state.Budget))
		for n := 0; n < count; n++ {
			state.Children = append(state.Children, agentdata.RunTask{TaskID: uint64(n + 1), RequirementsJSON: string(rawSpec)})
		}
		calls := []ToolCall{}
		for n := 0; n < 8; n++ {
			calls = append(calls, ToolCall{ID: fmt.Sprintf("latest-%d", n), Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"dom"}`}})
		}
		messages := []Message{{Role: "tool", Content: strings.Repeat("old", 24000)}, {Role: "assistant", ToolCalls: calls}}
		for n := 0; n < 8; n++ {
			out, _ := toolData(calls[n], map[string]any{"handle": "source", "view_version": "dom-structure-v1", "selector_catalog": []DOMBlock{{Selector: fmt.Sprintf(".observed-%d", n), Matches: 1, Sample: "short field"}}})
			raw, _ := json.Marshal(out)
			messages = append(messages, Message{Role: "tool", ToolCallID: calls[n].ID, Content: string(raw)})
		}
		got, err := CompactContext(state, messages)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(got)
		if strings.Contains(string(raw), "requirements_json") || !strings.Contains(string(raw), "frozen_spec") || !strings.Contains(string(raw), "task_id") {
			t.Fatal("requirements duplicated or child identities lost")
		}
	}
}

func TestCompactionReplaysObservedResearchFailure(t *testing.T) {
	path := os.Getenv("GORELAY_CONTEXT_REPLAY")
	if path == "" {
		t.Skip("opt-in captured API replay")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var captured struct {
		Run        agentdata.Run         `json:"run"`
		Operations []agentdata.Operation `json:"operations"`
		Children   []ChildResult         `json:"children"`
	}
	if err := json.Unmarshal(raw, &captured); err != nil {
		t.Fatal(err)
	}
	state := LoopState{Run: captured.Run, Operations: captured.Operations, TaskResults: captured.Children}
	for _, child := range captured.Children {
		state.Children = append(state.Children, child.Link)
	}
	var spec TaskSpec
	json.Unmarshal([]byte(captured.Run.SpecJSON), &spec)
	state.Spec = &spec
	json.Unmarshal([]byte(captured.Run.BudgetJSON), &state.Budget)
	var messages []Message
	json.Unmarshal([]byte(captured.Run.PlanJSON), &messages)
	got, err := CompactContext(state, messages)
	if err != nil {
		t.Fatal(err)
	}
	packed, _ := json.Marshal(got)
	if len(packed) > 64<<10 {
		t.Fatal("replayed observation still cannot fit")
	}
	t.Logf("captured legal round now fits in %d bytes", len(packed))
}

func TestCompactionRetainsLatestCompletedFeedbackNotJustItsPreview(t *testing.T) {
	for _, tool := range []string{"read_source_content", "validate_program"} {
		messages := []Message{{Role: "system", Content: AgentSystemPrompt}, {Role: "user", Content: "original goal"}, {Role: "assistant", Content: "older round"}, {Role: "tool", Content: strings.Repeat("old", 24000)}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "latest", Type: "function", Function: FunctionCall{Name: tool, Arguments: `{}`}}}}, {Role: "tool", ToolCallID: "latest", Content: `{"tool_call_id":"latest","name":"` + tool + `","data":{"view_version":"plain-v1","text":"CURRENT_COMPLETE_FEEDBACK"}}`}, {Role: "system", Content: "current coverage"}}
		got, err := CompactContext(LoopState{Run: agentdata.Run{Goal: "original goal"}}, messages)
		raw, _ := json.Marshal(got)
		if err != nil || !strings.Contains(string(raw), "CURRENT_COMPLETE_FEEDBACK") || !strings.Contains(string(raw), "current coverage") {
			t.Fatalf("latest completed feedback lost: %s %v", raw, err)
		}
	}
}

func TestAcknowledgedHistoryLeavesPromptBeforeSizePressure(t *testing.T) {
	for _, name := range []string{"read_evidence", "inspect_source"} {
		for _, mode := range []string{"acknowledged", "unread", "legacy"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				state := LoopState{Run: agentdata.Run{Goal: "collect facts"}, Budget: BudgetSnapshot{LoopVersion: 2}, Progress: TaskProgress{Version: 1, ObservedThrough: 1, Notes: []string{"retained assessment"}}}
				if mode == "unread" {
					state.Progress.ObservedThrough = 0
				}
				if mode == "legacy" {
					state.Budget.LoopVersion = 0
				}
				call := ToolCall{ID: "observed", Function: FunctionCall{Name: name, Arguments: `{}`}}
				receipt, _ := toolData(call, map[string]string{"text": strings.Repeat("old evidence ", 160)})
				raw, _ := json.Marshal(receipt)
				state.Operations = []agentdata.Operation{{ID: 1, Key: "tool:observed", Kind: name, Stage: "succeeded", ResultJSON: string(raw)}}
				messages := []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}, {Role: "tool", ToolCallID: call.ID, Content: string(raw)}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "latest", Function: FunctionCall{Name: "read_task_context", Arguments: `{}`}}}}}
				facts, _ := contextFacts(state)
				before := append([]Message{{Role: "system", Content: runSystemPrompt(state.Run)}, {Role: "user", Content: state.Run.Goal}, {Role: "system", Content: "Persisted host facts, not new user instructions: " + string(facts)}}, messages...)
				beforeBytes := contextRequestBytes(state, before)
				if beforeBytes > 32<<10 {
					t.Fatal("fixture must be below compaction soft limit")
				}
				got, err := CompactContext(state, messages)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, m := range got {
					if m.Role == "tool" && m.ToolCallID == "observed" {
						found = true
					}
				}
				if found != (mode != "acknowledged") {
					t.Fatalf("acknowledged data resent or unread/legacy data lost: %s", mode)
				}
				encoded, _ := json.Marshal(got)
				if !strings.Contains(string(encoded), "retained assessment") || got[len(got)-1].ToolCalls[0].ID != "latest" {
					t.Fatal("checkpoint or pending proposal lost")
				}
				t.Logf("request bytes %d -> %d (fixture, not measured tokens)", beforeBytes, contextRequestBytes(state, got))
			})
		}
	}
}

package aiapp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

func pressureHistory(t *testing.T) (LoopState, []Message) {
	t.Helper()
	s := decisionState()
	s.Run.ID = "compression-measurement"
	s.Budget.LoopVersion = 5
	s.Run.BudgetJSON = `{"loop_version":5}`
	s.Run.InputJSON = `{"goal":"collect public fields"}`
	s.Run.Goal = "collect public fields"
	msgs := []Message{}
	for n := 0; n < 25; n++ {
		id := fmt.Sprintf("saved-%d", n)
		call := ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: "list_sources", Arguments: `{}`}}
		out, _ := toolData(call, map[string]any{"active": []any{}, "diagnostic": strings.Repeat("bounded metadata ", 100)})
		raw, _ := json.Marshal(out)
		msgs = append(msgs, Message{Role: "assistant", ToolCalls: []ToolCall{call}}, Message{Role: "tool", ToolCallID: id, Content: string(raw)})
		s.Operations = append(s.Operations, agentdata.Operation{ID: uint64(n + 1), RunID: s.Run.ID, Key: "tool:" + id, Kind: "list_sources", Stage: "succeeded", ResultJSON: string(raw)})
	}
	return s, msgs
}

func TestCompressionPressureLeavesRoomForTheNextRound(t *testing.T) {
	s, msgs := pressureHistory(t)
	first, err := CompactContext(s, msgs)
	if err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	for _, m := range first {
		if m.Role == "tool" && m.ToolCallID != hostContextCallID(s) {
			kept[m.ToolCallID] = true
		}
	}
	call := ToolCall{ID: "next-small-round", Type: "function", Function: FunctionCall{Name: "list_sources", Arguments: `{}`}}
	out, _ := toolData(call, map[string]any{"active": []any{}, "diagnostic": strings.Repeat("new metadata ", 70)})
	raw, _ := json.Marshal(out)
	first = append(first, Message{Role: "assistant", ToolCalls: []ToolCall{call}}, Message{Role: "tool", ToolCallID: call.ID, Content: string(raw)})
	s.Operations = append(s.Operations, agentdata.Operation{ID: 26, RunID: s.Run.ID, Key: "tool:" + call.ID, Kind: call.Function.Name, Stage: "succeeded", ResultJSON: string(raw)})
	second, err := CompactContext(s, first)
	if err != nil {
		t.Fatal(err)
	}
	visible := map[string]bool{}
	for _, m := range second {
		visible[m.ToolCallID] = true
	}
	for id := range kept {
		if !visible[id] {
			t.Fatalf("a small next round forced compression again immediately: evicted %s", id)
		}
	}
}

func TestCompressionMeasuresPlainAssistantHistory(t *testing.T) {
	s, _ := pressureHistory(t)
	before := []Message{{Role: "assistant", Content: "A completed decision without a tool call."}}
	p := &ContextCompactionProfile{HistoryMessagesBefore: 1}
	finishContextMeasurement(s, before, before, p)
	if p.HistoryMessagesAfter != 1 || p.Triggered {
		t.Fatalf("unchanged plain assistant decision counted as eviction: %+v", p)
	}
}

func TestCompressionMeasuresLostStructuralValuesAndOriginalGoal(t *testing.T) {
	state := efficiencyState()
	state.Run.Goal = "deliver the requested original task"
	for _, node := range []map[string]any{
		{"index": 0, "text": "项目编号 CG-42", "attributes": map[string]any{}, "links": []any{map[string]any{"href": "https://example.test/detail/42"}}},
		{"index": 1, "text": "License CC-BY-4.0", "attributes": map[string]any{"id": "license"}},
	} {
		call := ToolCall{ID: "read-observed", Function: FunctionCall{Name: "read_data"}}
		full, _ := toolData(call, map[string]any{"handle": "source", "view_version": "dom-structure-v1", "selector": ".entries > li", "nodes": []any{node}})
		empty, _ := toolData(call, map[string]any{"handle": "source", "view_version": "dom-structure-v1", "selector": ".entries > li"})
		before := []Message{{Role: "user", Content: state.Run.Goal}, {Role: "tool", ToolCallID: call.ID, Content: string(mustJSON(full))}}
		after := []Message{{Role: "tool", ToolCallID: call.ID, Content: string(mustJSON(empty))}}
		p := &ContextCompactionProfile{}
		finishContextMeasurement(state, before, after, p)
		if p.Facts["node_value"].Lost != 1 || p.Facts["original_goal"].Lost != 1 {
			t.Fatal("lost values/goal were falsely reported as no information loss", p.Facts)
		}
		unchanged := &ContextCompactionProfile{}
		finishContextMeasurement(state, before, before, unchanged)
		if unchanged.Facts["node_value"].Retention != 1 || unchanged.Facts["original_goal"].Retention != 1 {
			t.Fatal("unchanged data counted as lost", unchanged.Facts)
		}
	}
}

func TestCompressionRetainsExecutableCrawlerFacts(t *testing.T) {
	s, pressure := pressureHistory(t)
	laterOps := s.Operations
	s.Operations = nil
	s.Spec = &TaskSpec{Goal: s.Run.Goal, RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}}
	msgs := []Message{}
	add := func(id, name, args string, data any, failure bool) {
		call := ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: args}}
		out, _ := toolData(call, data)
		stage := "succeeded"
		if failure {
			stage = "failed"
			out.Error = "url is required"
			out.Failure = &FailureInfo{Code: "MISSING_REQUIRED_PARAMETER", Details: map[string]any{"field": "url"}}
		}
		raw, _ := json.Marshal(out)
		msgs = append(msgs, Message{Role: "assistant", ToolCalls: []ToolCall{call}}, Message{Role: "tool", ToolCallID: id, Content: string(raw)})
		s.Operations = append(s.Operations, agentdata.Operation{ID: uint64(100 + len(s.Operations)), RunID: s.Run.ID, Key: "tool:" + id, Kind: name, Stage: stage, ResultJSON: string(raw)})
	}
	add("read-early", "read_data", `{"handle":"original-source","view":"dom","selector":".entries > li"}`, map[string]any{"handle": "original-source", "selector": ".entries > li", "view_version": "dom-structure-v1", "next_read": map[string]any{"handle": "original-source", "view": "dom", "selector": ".entries > li", "offset": 64, "limit": 64}}, false)
	add("validate-early", "validate_program", `{}`, map[string]any{"id": "candidate-early", "artifact_hash": "hash-early", "next_action": map[string]any{"tool": "register_source", "arguments": map[string]any{"candidate_id": "candidate-early"}}}, false)
	add("publish-early", "register_source", `{}`, map[string]any{"source_id": "published-early", "artifact_hash": "hash-early"}, false)
	add("error-early", "inspect_source", `{}`, nil, true)
	s.Operations = append(s.Operations, laterOps...)
	msgs = append(msgs, pressure...)
	for i := range msgs {
		if msgs[i].Role == "assistant" {
			msgs[i].Content = "unverified model plan repeated in the old checkpoint"
		}
	}
	out, p, err := compactContextMeasured(s, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Triggered || p.EvictedRounds == 0 || p.AfterBytes > 64<<10 {
		t.Fatalf("did not measure bounded history eviction: %+v", p)
	}
	if len(p.MissingCriticalFacts) != 0 {
		t.Errorf("lost executable crawler facts: %+v", p)
	}
	for _, kind := range []string{"candidate_id", "source_id", "continuation", "required_field", "diagnostic"} {
		if f := p.Facts[kind]; f.Before == 0 || f.Retention != 1 {
			t.Errorf("unmeasured or lost %s: %+v", kind, f)
		}
	}
	if f := p.Facts["selector"]; f.Before != 1 || f.Retention != 1 {
		t.Fatalf("lost selected locator: %+v", f)
	}
	for _, m := range out {
		if m.ToolCallID != hostContextCallID(s) {
			continue
		}
		var snapshot map[string]json.RawMessage
		json.Unmarshal([]byte(m.Content), &snapshot)
		if strings.Contains(string(snapshot["history_checkpoint"]), "model_note_unverified") {
			t.Error("crawler checkpoint recopied model prose instead of indexing host outcomes")
		}
	}
	profile, _ := json.Marshal(p)
	t.Log(string(profile))
}

package aiapp

import (
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/knowledge"
	"strings"
	"testing"
	"time"
)

func TestTransportFailureAndCooldownShareActionableEnvelope(t *testing.T) {
	for _, err := range []error{&providerCooldown{Until: time.Now().Add(time.Minute)}, &ProviderError{Status: 503, Retryable: true}, ErrProviderQuota} {
		f := captureFailure(err, 0, false)
		if f == nil || f.Category == "" || f.Code == "" || f.ExecutionStatus == "" || f.Recovery == nil {
			t.Fatalf("incomplete failure envelope: %+v", f)
		}
	}
}

func TestEvidenceReferenceFailureRequestsFreshRetrieval(t *testing.T) {
	f := captureFailure(knowledge.ErrInvalidReference, 0, false)
	raw, _ := json.Marshal(f)
	if !strings.Contains(string(raw), `"strategy":"refresh_and_retrieve"`) || !strings.Contains(string(raw), `"tool":"search_knowledge"`) {
		t.Fatalf("invalid reference recovery is not actionable: %s", raw)
	}
	failed := ToolResult{CallID: "old-read", Name: "read_data", Error: knowledge.ErrInvalidReference.Error(), Failure: f}
	req := feedbackRequest(map[string]any{"children": []any{map[string]any{"id": "child"}}, "coverage": map[string]any{"satisfied": true}, "frozen_spec": map[string]any{"require_summary": true}}, failed)
	// Evidence identity comes from the real proposal, never a private lookup.
	req.Messages = append([]Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "old-read", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"ev_old","pointer":"/text"}`}}}}}, req.Messages...)
	out, err := correctChainDecision(req)
	if err != nil || len(out.Message.ToolCalls) != 1 || out.Message.ToolCalls[0].Function.Name != "search_knowledge" {
		t.Fatalf("correct model cannot recover reference invalidation: %+v %v", out, err)
	}
	latest, _ := json.Marshal(ToolResult{Name: "search_knowledge", CallID: "new-search", Data: json.RawMessage(`{"entries":[{"id":"ev_current","text":"budget"}]}`)})
	req.Messages = append(req.Messages, Message{Role: "tool", Content: string(latest)})
	out, err = correctChainDecision(req)
	if err != nil || len(out.Message.ToolCalls) != 1 || out.Message.ToolCalls[0].Function.Name != "read_data" || !strings.Contains(out.Message.ToolCalls[0].Function.Arguments, "ev_current") {
		t.Fatalf("resolved error overrode fresh retrieval: %+v %v", out, err)
	}
}

func TestToolFailureProvidesActionableTypedContract(t *testing.T) {
	for _, field := range []string{"detail_limit", "cursor"} {
		err := &ParameterError{Reason: "missing required parameter " + field, Required: []string{field}, Accepted: map[string]string{field: "string"}, Example: map[string]any{field: "observed"}}
		f := captureFailure(err, 0, false)
		if f == nil {
			t.Fatal("ordinary tool failure lacks structured recovery")
		}
		raw, _ := json.Marshal(f)
		var value map[string]any
		json.Unmarshal(raw, &value)
		if value["category"] != "validation" || value["recovery"] == nil || value["details"] == nil {
			t.Fatalf("correction information missing: %s", raw)
		}
		if f.stops() {
			t.Fatal("correctable arguments must not stop entire run")
		}
	}
	f := captureFailure(errors.New("execution acknowledgement missing"), 0, false)
	if f == nil {
		t.Fatal("unknown error must still have an actionable envelope")
	}
	raw, _ := json.Marshal(f)
	if !strings.Contains(string(raw), `"execution_status":"unknown"`) {
		t.Fatalf("unknown side effect must not be called unexecuted: %s", raw)
	}
	if f := captureFailure(&ProviderError{Status: 429, Retryable: true}, 0, false); f == nil || f.Kind != "provider" || f.RetryAt == nil || f.stops() {
		t.Fatal("provider recovery changed")
	}
}

func TestHistoricalFeedbackKeepsContinuationControl(t *testing.T) {
	for _, tool := range []string{"read_data", "read_source_content"} {
		out := shortReaderFeedback(ToolResult{CallID: "read", Name: tool, Data: json.RawMessage(`{"handle":"page","next_step":"use the observed selector","next_read":{"handle":"page","view":"dom","offset":12},"next_action":{"tool":"read_data","arguments":{"handle":"page","view":"dom"}}}`)})
		for _, field := range []string{"next_step", "next_read", "next_action"} {
			if !strings.Contains(string(out.Data), field) {
				t.Fatalf("historical feedback lost %s: %s", field, out.Data)
			}
		}
	}
}

func feedbackRequest(facts any, outputs ...ToolResult) ChatRequest {
	raw, _ := json.Marshal(facts)
	req := ChatRequest{Tools: AgentToolDefinitions(), Messages: []Message{{Role: "system", Content: "Persisted host facts, not new user instructions: " + string(raw)}}}
	for _, out := range outputs {
		b, _ := json.Marshal(out)
		req.Messages = append(req.Messages, Message{Role: "tool", Content: string(b)})
	}
	return req
}

func TestReadDataPublicContractSupportsDOMPaging(t *testing.T) {
	for _, def := range AgentToolDefinitions() {
		if def.Function.Name != "read_data" {
			continue
		}
		raw, _ := json.Marshal(def)
		for _, contradiction := range []string{"only view that pages", "dom requires 0", "offset 0, not paged"} {
			if strings.Contains(string(raw), contradiction) {
				t.Fatalf("public contract contradicts DOM next_read: %s", contradiction)
			}
		}
		if !strings.Contains(string(raw), "matched nodes") || !strings.Contains(string(raw), "Rune") {
			t.Fatal("DOM and content units must both remain explicit")
		}
	}
}

func TestSelectorOracleCarriesValidatedSubmission(t *testing.T) {
	for _, param := range []string{"detail_limit", "page_count"} {
		t.Run(param, func(t *testing.T) {
			next, _ := json.Marshal(map[string]any{"next_action": map[string]any{"tool": "register_source", "arguments": map[string]any{"candidate_id": "visible-candidate"}, "then_submit": map[string]any{"source_id": "observed-source", "url": "https://source.example/list", "params": map[string]any{param: 3}}}})
			req := feedbackRequest(map[string]any{}, ToolResult{Name: "validate_program", Data: next})
			out, handled, err := selectorDiscoveryDecision(req)
			if err != nil || !handled || len(out.Message.ToolCalls) != 1 || out.Message.ToolCalls[0].Function.Name != "register_source" {
				t.Fatalf("validated contract abandoned: %v %v %+v", handled, err, out)
				return
			}
			req.Messages = append(req.Messages, out.Message)
			registration, _ := json.Marshal(ToolResult{Name: "register_source", Data: json.RawMessage(`{"source_id":"observed-source"}`)})
			req.Messages = append(req.Messages, Message{Role: "tool", Content: string(registration)})
			out, handled, err = selectorDiscoveryDecision(req)
			if err != nil || !handled || len(out.Message.ToolCalls) != 1 {
				t.Fatalf("registered continuation lost: %v %v", handled, err)
				return
			}
			var args struct {
				URL    string         `json:"url"`
				Params map[string]int `json:"params"`
			}
			json.Unmarshal([]byte(out.Message.ToolCalls[0].Function.Arguments), &args)
			if out.Message.ToolCalls[0].Function.Name != "submit_acquisition" || args.URL != "https://source.example/list" || args.Params[param] != 3 {
				t.Fatalf("validated params lost: %+v", out)
			}
		})
	}
	// Pending execution is handled by the existing completion oracle.
	_, handled, err := selectorDiscoveryDecision(feedbackRequest(map[string]any{"children": []any{map[string]any{"id": "child"}}}))
	if err != nil || handled {
		t.Fatal("must preserve child completion handoff")
	}
}

func TestCitedCompletionIgnoresUnrelatedDOMReceipt(t *testing.T) {
	for _, children := range []bool{true, false} {
		t.Run(map[bool]string{true: "acquired", false: "reused"}[children], func(t *testing.T) {
			facts := map[string]any{"coverage": map[string]any{"satisfied": true}, "frozen_spec": map[string]any{"require_summary": true}}
			outputs := []ToolResult{{Name: "read_data", Data: json.RawMessage(`{"handle":"source-page","selector_catalog":[]}`)}}
			if children {
				facts["children"] = []any{map[string]any{"id": "child"}}
			} else {
				outputs = append(outputs, ToolResult{Name: "reuse_knowledge", Data: json.RawMessage(`{}`)})
			}
			out, err := correctChainDecision(feedbackRequest(facts, outputs...))
			if err != nil || len(out.Message.ToolCalls) != 1 || out.Message.ToolCalls[0].Function.Name != "search_knowledge" {
				t.Fatalf("source DOM mistaken for evidence or summary skipped: %+v %v", out, err)
			}
		})
	}
}

func TestIndependentOracleUsesPublishedParameterExample(t *testing.T) {
	for _, name := range []string{"detail_limit", "page_count"} {
		item := SourceCapability{InputExampleJSON: `{"` + name + `":3}`}
		item.Source.SourceID = "audit-source"
		catalog, _ := json.Marshal(map[string]any{"active": []SourceCapability{item}})
		req := feedbackRequest(map[string]any{}, ToolResult{Name: "list_sources", Data: catalog})
		req.Messages = append(req.Messages, Message{Role: "user", Content: `{"goal":"refresh https://source.example/new-list"}`})
		out, err := correctChainDecision(req)
		if err != nil {
			t.Fatal(err)
		}
		var args struct {
			URL    string         `json:"url"`
			Params map[string]int `json:"params"`
		}
		json.Unmarshal([]byte(out.Message.ToolCalls[0].Function.Arguments), &args)
		if args.URL != "https://source.example/new-list" || args.Params[name] != 3 {
			t.Fatalf("independent reuse guessed parameters: %+v", args)
		}
	}
}

func TestOracleRefreshesKnowledgeThatNoLongerMeetsFreshness(t *testing.T) {
	item := SourceCapability{InputExampleJSON: `{"detail_limit":3}`}
	item.Source.SourceID = "audit-source"
	catalog, _ := json.Marshal(map[string]any{"active": []SourceCapability{item}})
	req := feedbackRequest(map[string]any{"coverage": map[string]any{"satisfied": false}}, ToolResult{Name: "reuse_knowledge", Data: json.RawMessage(`{}`)}, ToolResult{Name: "list_sources", Data: catalog})
	out, err := correctChainDecision(req)
	if err != nil || len(out.Message.ToolCalls) != 1 || out.Message.ToolCalls[0].Function.Name != "submit_acquisition" {
		t.Fatalf("stale reuse must refresh instead of claiming delivery: %+v %v", out, err)
	}
}

func TestIdealMockUsesCurrentReceiptBeforeHistoricalFailure(t *testing.T) {
	for _, name := range []string{"validate_program", "submit_acquisition"} {
		old := ToolResult{Name: name, CallID: "old", Error: "old rejected arguments"}
		current := ToolResult{Name: name, CallID: "corrected", Data: json.RawMessage(`{"status":"accepted"}`)}
		req := feedbackRequest(map[string]any{}, old, current)
		snapshot, _ := json.Marshal(map[string]any{"recent_tool_feedback": []ToolResult{old}})
		req.Messages = append(req.Messages, Message{Role: "tool", Content: string(snapshot)})
		got, _ := auditVisibleReceipts(req)
		if got[name].CallID != "corrected" {
			t.Fatal("old memory overwrote current correction", got[name])
		}
	}
	newest := ToolResult{Name: "read_data", CallID: "newest"}
	old := ToolResult{Name: "read_data", CallID: "old"}
	snapshot, _ := json.Marshal(map[string]any{"recent_tool_feedback": []ToolResult{newest, old}})
	got, _ := auditVisibleReceipts(ChatRequest{Messages: []Message{{Role: "tool", Content: string(snapshot)}}})
	if got["read_data"].CallID != "newest" {
		t.Fatal("reverse chronological memory was read backwards")
	}
}

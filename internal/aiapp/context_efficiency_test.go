package aiapp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

func efficiencyState() LoopState {
	s := decisionState()
	s.Run.ID = "context-efficiency"
	s.Run.InputJSON = `{"goal":"collect"}`
	s.Run.BudgetJSON = `{"loop_version":5}`
	s.Budget.LoopVersion = 5
	return s
}

func appendEfficiencyRound(s *LoopState, name string, data any, failure string) {
	id := fmt.Sprintf("round-%d", len(s.Operations))
	call := ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: name, Arguments: `{}`}}
	rawData, _ := json.Marshal(data)
	receipt := ToolResult{CallID: id, Name: name, Data: rawData, Error: failure}
	raw, _ := json.Marshal(receipt)
	stage := "succeeded"
	if failure != "" {
		stage = "failed"
	}
	s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}}, Message{Role: "tool", ToolCallID: id, Content: string(raw)})
	s.Operations = append(s.Operations, agentdata.Operation{ID: uint64(len(s.Operations) + 1), RunID: s.Run.ID, Key: "tool:" + id, Kind: name, Stage: stage, ResultJSON: string(raw)})
}

func TestCachePrefixIncludesCompletedHistoryBeforeVolatileFacts(t *testing.T) {
	for _, name := range []string{"read_data", "list_sources"} {
		t.Run(name, func(t *testing.T) {
			s := efficiencyState()
			appendEfficiencyRound(&s, name, map[string]any{"view_version": "plain-v1", "text": "observed fact"}, "")
			pending := Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "pending", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source"}`}}}}
			s.Messages = append(s.Messages, pending)
			a, err := CompactContext(s, s.Messages)
			if err != nil {
				t.Fatal(err)
			}
			s.Budget.Spent.ModelCalls++
			b, err := CompactContext(s, s.Messages)
			if err != nil {
				t.Fatal(err)
			}
			if len(a) < 4 || a[2].Role != "assistant" || a[2].ToolCalls[0].ID != "round-0" {
				t.Fatal("volatile facts still precede completed cacheable history")
			}
			if string(mustJSON(a[:4])) != string(mustJSON(b[:4])) {
				t.Fatal("host counters changed completed prefix")
			}
			if a[len(a)-1].ToolCalls[0].ID != "pending" {
				t.Fatal("pending proposal moved away from execution tail")
			}
		})
	}
}

func TestConsumedReceiptsStayIntactBeforeWindowPressure(t *testing.T) {
	for _, kind := range []string{"read_data", "get_task_results"} {
		t.Run(kind, func(t *testing.T) {
			s := efficiencyState()
			bulk := strings.Repeat("preserved control metadata ", 70)
			for n := 0; n < 7; n++ {
				data := map[string]any{"view_version": "dom-structure-v1", "handle": fmt.Sprintf("source-%d", n), "selector": fmt.Sprintf(".fact-%d", n), "structure_note": bulk, "next_read": map[string]any{"handle": fmt.Sprintf("source-%d", n), "view": "dom", "offset": 777}}
				if kind == "get_task_results" {
					data = map[string]any{"records": []any{map[string]any{"fields": map[string]any{"title": fmt.Sprintf("fact-%d", n)}}}, "task_id": n + 1, "structure_note": bulk}
				}
				appendEfficiencyRound(&s, kind, data, "")
			}
			appendEfficiencyRound(&s, "read_data", nil, `selector .observed-title matched 0 nodes; available: h6.title`)
			before := string(mustJSON(s.Messages))
			got, err := CompactContext(s, s.Messages)
			if err != nil {
				t.Fatal(err)
			}
			raw := string(mustJSON(got))

			var first string
			for _, m := range got {
				if m.ToolCallID == "round-0" {
					first = m.Content
				}
			}
			if first == "" || !strings.Contains(first, bulk) || !strings.Contains(first, "fact-0") {
				t.Fatal("receipt was shortened without context pressure")
			}
			if !strings.Contains(raw, "h6.title") || !strings.Contains(raw, "source-0") && kind == "read_data" {
				t.Fatal("error or source continuation identity lost")
			}
			if before != string(mustJSON(s.Messages)) {
				t.Fatal("compaction mutated persisted history")
			}
		})
	}
}

func TestEvictedHistoryKeepsCheckpointAsUntrustedData(t *testing.T) {
	s := efficiencyState()
	// Explicitly force pressure in this test; normal runs use the much larger frozen window.
	s.Budget.PromptPolicy = &PromptPolicy{WindowTokens: 32000, CompactAtTokens: 12000, CompactToTokens: 8000, MaxOutputTokens: 8000}
	s.Run.BudgetJSON = string(mustJSON(s.Budget))
	for n := 0; n < 12; n++ {
		appendEfficiencyRound(&s, "read_data", map[string]any{"view_version": "dom-structure-v1", "handle": "source", "selector_catalog": []DOMBlock{{Selector: fmt.Sprintf(".observed-%d", n), Matches: 1, Sample: "observed label"}}}, "")
		s.Messages[len(s.Messages)-2].Content = "MODEL_NOTE_UNVERIFIED " + strings.Repeat("historical observation ", 250)
	}
	pending := Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "pending", Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: `{"code":"original pending code"}`}}}}
	s.Messages = append(s.Messages, pending)
	got, err := CompactContext(s, s.Messages)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range got {
		if m.Role == "system" && strings.Contains(m.Content, "MODEL_NOTE_UNVERIFIED") {
			t.Fatal("model notes promoted to trusted instructions")
		}
		if m.Role == "tool" && m.ToolCallID == hostContextCallID(s) && strings.Contains(m.Content, "history_checkpoint") && strings.Contains(m.Content, "round-") {
			found = true
		}
	}
	if !found {
		t.Fatal("eviction lost recoverable task checkpoint")
	}
	continued, err := CompactContext(s, got)
	if err != nil {
		t.Fatal(err)
	}
	checkpointSurvived := false
	for _, m := range continued {
		if m.ToolCallID == hostContextCallID(s) && strings.Contains(m.Content, "history_checkpoint") && strings.Contains(m.Content, "round-") {
			checkpointSurvived = true
		}
	}
	if !checkpointSurvived {
		t.Fatal("next compaction lost the crawler receipt index after removing its original messages")
	}
	if got[len(got)-1].ToolCalls[0].Function.Arguments != pending.ToolCalls[0].Function.Arguments {
		t.Fatal("pending program altered")
	}
	if estimated, _ := estimateContextTokens(s, got); estimated > s.Budget.PromptPolicy.WindowTokens-s.Budget.PromptPolicy.MaxOutputTokens {
		t.Fatal("checkpoint exceeds frozen token budget")
	}
}

func TestCheckpointDoesNotReintroduceArchivedReceiptBulk(t *testing.T) {
	s := efficiencyState()
	for n := 0; n < 8; n++ {
		appendEfficiencyRound(&s, "inspect_source", map[string]any{"source": map[string]any{"receipt": map[string]any{"url": fmt.Sprintf("https://fixture.example/%d", n)}, "body": strings.Repeat("already archived prose ", 300)}, "selector_catalog": []any{map[string]any{"selector": "article", "sample": strings.Repeat("repeated structure ", 170)}}}, "")
	}
	checkpoint := historyCheckpoint(s.Messages, nil)
	if len(mustJSON(checkpoint)) > 8<<10 {
		t.Fatal("checkpoint copied bulky page/selector views back into host memory")
	}
}

func TestConsumedStructureRemainsActionableWithoutAnotherRead(t *testing.T) {
	for _, name := range []string{"read_data", "inspect_source"} {
		t.Run(name, func(t *testing.T) {
			receipt := ToolResult{CallID: "observed", Name: name}
			receipt.Data, _ = json.Marshal(map[string]any{"text": strings.Repeat("long source prose ", 200), "matches": 2, "nodes": []any{map[string]any{"selector": "a.notice", "attributes": map[string]any{"href": "/detail/7"}, "text": "actual notice"}}, "json_structure": map[string]any{"rows": "array"}, "complete_source_text": map[string]any{"field_locator": map[string]any{"source_text": true}}})
			view := consumedReceiptSummary(receipt)
			raw := string(view.Data)
			for _, required := range []string{"a.notice", "/detail/7", "actual notice", "json_structure", "source_text"} {
				if !strings.Contains(raw, required) {
					t.Fatalf("compaction discarded observed machine structure: %s", required)
				}
			}
			if strings.Contains(raw, strings.Repeat("long source prose ", 200)) {
				t.Fatal("prose was not reduced")
			}
		})
	}
}

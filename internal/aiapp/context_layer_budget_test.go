package aiapp

import (
	"encoding/json"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"strings"
	"testing"
)

func TestWorkingHistoryBudgetIsIndependentOfStaticToolSchema(t *testing.T) {
	for _, tag := range []string{"notice", "game"} {
		state := decisionState()
		state.Run.ID = "layer-budget"
		state.Budget.LoopVersion = 5
		state.Spec = &TaskSpec{Goal: "Extract three independent values", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}, JoinKeys: []string{"title"}}
		policy, err := configuredPromptPolicy()
		if err != nil {
			t.Fatal(err)
		}
		state.Budget.PromptPolicy = policy
		state.Run.BudgetJSON = string(mustJSON(state.Budget))
		var messages []Message
		for n := 0; n < 5; n++ {
			id := fmt.Sprintf("read-%d", n)
			call := ToolCall{ID: id, Function: FunctionCall{Name: "read_data", Arguments: string(mustJSON(map[string]any{"handle": "source", "view": "dom", "offset": n * 16}))}}
			blocks := []DOMBlock{}
			for field := 0; field < 16; field++ {
				blocks = append(blocks, DOMBlock{Selector: fmt.Sprintf(".%s-distinct-page-%d-field-%d", tag, n, field), Matches: 1, Sample: fmt.Sprintf("Observed field %d on structure page %d", field, n)})
			}
			out, _ := toolData(call, map[string]any{"handle": "source", "view_version": "dom-structure-v1", "selector_catalog": blocks, "next_read": map[string]any{"handle": "source", "view": "dom", "offset": (n + 1) * 16}})
			raw, _ := json.Marshal(out)
			messages = append(messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}}, Message{Role: "tool", ToolCallID: id, Content: string(raw)})
			state.Operations = append(state.Operations, agentdata.Operation{ID: uint64(n + 1), RunID: state.Run.ID, Key: "tool:" + id, Kind: "read_data", Stage: "succeeded", ResultJSON: string(raw)})
		}
		state.Messages = messages
		got, err := CompactContext(state, messages)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(got)
		t.Logf("whole=%d history=%d", contextRequestBytes(state, got), len(mustJSON(messages)))
		if !strings.Contains(string(encoded), tag+"-distinct-page-0") {
			t.Fatal("static prompt/tool overhead evicted a short working history")
		}
		if estimated, _ := estimateContextTokens(state, got); estimated > policy.WindowTokens-policy.MaxOutputTokens {
			t.Fatal("whole request exceeded frozen token window")
		}
	}
}

func mustJSON(value any) []byte { raw, _ := json.Marshal(value); return raw }

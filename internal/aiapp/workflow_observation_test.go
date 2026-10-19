package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"strings"
	"testing"
)

func TestWorkflowCompactionKeepsTheNextExecutableAction(t *testing.T) {
	for _, name := range []string{"validate_program", "submit_acquisition"} {
		t.Run(name, func(t *testing.T) {
			data := map[string]any{"candidate_id": "candidate-opaque", "artifact_hash": "hash-opaque", "source_id": "source-opaque", "kind": "adapter", "next_action": map[string]any{"tool": "register_source", "arguments": map[string]any{"candidate_id": "candidate-opaque", "description": "collect observed fields"}, "then_submit": map[string]any{"source_id": "source-opaque", "url": "https://example.test/list", "params": map[string]any{"limit": 3}}}, "result": map[string]any{"collection": map[string]any{"items": []any{map[string]any{"body": strings.Repeat("stored field", 5000)}}}}}
			out, _ := toolData(ToolCall{ID: "validation", Function: FunctionCall{Name: name}}, data)
			compact := observationWithinBudget(out, 1024)
			var control map[string]json.RawMessage
			json.Unmarshal(compact.Data, &control)
			for _, key := range []string{"candidate_id", "source_id", "kind", "next_action"} {
				if len(control[key]) == 0 {
					t.Fatalf("%s disappeared behind an unavailable prose reader: %s", key, compact.Data)
				}
			}
			if strings.Contains(string(compact.Data), "read_observation") || strings.Contains(string(compact.Data), "stored field") {
				t.Fatal("workflow compaction still uses a prose receipt", string(compact.Data))
			}
			s := efficiencyState()
			raw, _ := json.Marshal(out)
			s.Operations = []agentdata.Operation{{ID: 1, RunID: s.Run.ID, Key: "tool:validation", Kind: name, Stage: "succeeded", ResultJSON: string(raw)}}
			old, _ := json.Marshal(observationReceiptHandle(out))
			history := logicalHistory(s, []Message{{Role: "tool", ToolCallID: "validation", Content: string(old)}})
			if len(history) != 1 || !strings.Contains(history[0].Content, `"next_action"`) {
				t.Fatal("persisted pointer view was not rebuilt from its canonical receipt")
			}
		})
	}
}

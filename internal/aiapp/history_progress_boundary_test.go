package aiapp

import (
	"encoding/json"
	"testing"

	"go-lua-crawler/internal/agentdata"
)

func TestCurrentReaderRestoresCanonicalReceiptWithoutOldProposal(t *testing.T) {
	for _, version := range []string{"knowledge-evidence-v1", "html-information-v1"} {
		t.Run(version, func(t *testing.T) {
			canonical, _ := toolData(ToolCall{ID: "owned-read", Function: FunctionCall{Name: "read_data"}}, map[string]any{"handle": "ev_current", "view_version": version, "text": "current supported excerpt"})
			raw, _ := json.Marshal(canonical)
			state := LoopState{Run: agentdata.Run{ID: "current"}, Operations: []agentdata.Operation{{ID: 1, RunID: "current", Key: "tool:owned-read", Kind: "read_data", Stage: "succeeded", ResultJSON: string(raw)}}}
			projected := canonical
			projected.Data = json.RawMessage(`{"observation_truncated":true}`)
			out := historyReaderView(state, projected, ToolCall{})
			var data map[string]any
			_ = json.Unmarshal(out.Data, &data)
			if data["view_version"] != version || data["text"] != "current supported excerpt" || data["legacy_reader_unresolved"] != nil {
				t.Fatal("current bounded reading was replaced with an unresolved legacy receipt", string(out.Data))
			}
			state.Operations[0].RunID = "other"
			out = historyReaderView(state, projected, ToolCall{})
			if string(out.Data) == string(canonical.Data) {
				t.Fatal("foreign run's original receipt was restored")
			}
		})
	}
}

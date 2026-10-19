package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"testing"
)

func TestRepeatedRejectionMeansTheSameArguments(t *testing.T) {
	for _, same := range []bool{false, true} {
		state := decisionState()
		state.Run.ID = "reject-identities"
		for i := 0; i < 2; i++ {
			hash := "same-input"
			if !same && i == 1 {
				hash = "corrected-input"
			}
			raw, _ := json.Marshal(ToolResult{CallID: string(rune('a' + i)), Name: "read_data", Error: "selector not found"})
			state.Operations = append(state.Operations, agentdata.Operation{RunID: state.Run.ID, Key: "tool:" + string(rune('a'+i)), Kind: "read_data", Stage: "failed", InputHash: hash, ResultJSON: string(raw)})
		}
		if (repeatedToolRejection(state) != nil) != same {
			t.Fatal("different corrective proposals mislabeled as exact repeats", same)
		}
		if same {
			raw, _ := json.Marshal(ToolResult{CallID: "resolved", Name: "read_data", Data: json.RawMessage(`{"text":"read succeeded"}`)})
			state.Operations = append(state.Operations, agentdata.Operation{RunID: state.Run.ID, Key: "tool:resolved", Kind: "read_data", Stage: "succeeded", InputHash: "different-input", ResultJSON: string(raw)})
			if repeatedToolRejection(state) == nil {
				t.Fatal("unrelated successful read erased another unresolved failure")
			}
			state.Operations[len(state.Operations)-1].InputHash = "same-input"
			if repeatedToolRejection(state) != nil {
				t.Fatal("resolved call still warned as impossible to execute")
			}
		}
	}
}

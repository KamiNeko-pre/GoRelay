package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"strings"
	"testing"
)

func TestRecentFeedbackRetainsDistinctFailuresAndLargeAssetIdentity(t *testing.T) {
	state := decisionState()
	state.Run.ID = "feedback-memory"
	state.Run.BudgetJSON = `{"loop_version":5}`
	for i, name := range []string{"read_data", "read_data", "validate_program"} {
		id := string(rune('a' + i))
		out := ToolResult{CallID: id, Name: name}
		stage := "failed"
		if name == "read_data" {
			out.Error = "missing selector for source " + id
		} else {
			stage = "succeeded"
			out.Data, _ = json.Marshal(map[string]any{"candidate_id": "asset-identity", "next_action": map[string]any{"tool": "submit_acquisition", "arguments": map[string]string{"candidate_id": "asset-identity"}}, "body": strings.Repeat("known source ", 1400)})
		}
		raw, _ := json.Marshal(out)
		state.Operations = append(state.Operations, agentdata.Operation{RunID: state.Run.ID, Key: "tool:" + id, Kind: name, Stage: stage, InputHash: id, ResultJSON: string(raw)})
	}
	foreign := state.Operations[0]
	foreign.RunID = "foreign"
	state.Operations = append(state.Operations, foreign)
	raw, _ := json.Marshal(recentToolFeedback(state))
	for _, value := range []string{"source a", "source b", "asset-identity"} {
		if !strings.Contains(string(raw), value) {
			t.Fatalf("feedback memory silently lost %q: %s", value, raw)
		}
	}
	if len(raw) > 12<<10 {
		t.Fatal("recovery memory exceeded its aggregate window")
	}
}

func TestProbeProgressIgnoresReceiptCountersAndDuplicateOutputs(t *testing.T) {
	state := decisionState()
	add := func(id string, used int, item string) {
		data, _ := json.Marshal(map[string]any{"mode": "probe", "rows": 1, "produced_fields": []string{"link"}, "items": []any{map[string]any{"link": item}}, "probes_used": used, "stats": map[string]any{"elapsed_ms": used * 20}})
		raw, _ := json.Marshal(ToolResult{CallID: id, Name: "validate_program", Data: data})
		state.Operations = append(state.Operations, agentdata.Operation{Kind: "validate_program", Stage: "succeeded", ResultJSON: string(raw)})
	}
	add("first", 1, "https://one.example/detail")
	before := probeSignature(state)
	add("repeat", 2, "https://one.example/detail")
	if probeSignature(state) != before {
		t.Fatal("receipt counters/duplicate observations invented progress")
	}
	add("new-source", 3, "https://two.example/detail")
	if probeSignature(state) == before {
		t.Fatal("genuinely new probe output did not count as preparation")
	}
}

func TestPointerFailureNamesObservedParentAndAvailableFields(t *testing.T) {
	for _, name := range []string{"project", "game"} {
		value := map[string]any{name: map[string]any{"title": "source fact", "link": "https://example.com/item"}}
		_, err := jsonPointer(value, "/"+name+"/guessed")
		if err == nil || !strings.Contains(err.Error(), "title") || !strings.Contains(err.Error(), "/"+name) {
			t.Fatal("missing path feedback gives nothing to correct", err)
		}
		actual, err := jsonPointer(value, "/"+name+"/title")
		if err != nil || actual != "source fact" {
			t.Fatal("observed pointer was broken")
		}
	}
}

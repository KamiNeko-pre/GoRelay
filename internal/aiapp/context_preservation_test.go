package aiapp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWorkingContextPreservesEvidenceWithoutPressure(t *testing.T) {
	for _, tool := range []string{"read_data", "get_task_results"} {
		t.Run(tool, func(t *testing.T) {
			s := efficiencyState()
			policy, err := configuredPromptPolicy()
			if err != nil {
				t.Fatal(err)
			}
			s.Budget.PromptPolicy = policy
			s.Run.BudgetJSON = string(mustJSON(s.Budget))
			// Preserve legitimate crawler observations, not a retired prose view.
			data := map[string]any{"view_version": "dom-structure-v1", "handle": "source", "selector_catalog": []DOMBlock{{Selector: ".CRITICAL_END_FACT", Matches: 1, Sample: "observed field label"}}, "next_read": map[string]any{"handle": "source", "view": "dom", "offset": 16}}
			if tool == "get_task_results" {
				data = map[string]any{"records": []any{map[string]any{"fields": map[string]any{"title": "CRITICAL_END_FACT"}}}}
			}
			appendEfficiencyRound(&s, tool, data, "")
			for i := 0; i < 4; i++ {
				appendEfficiencyRound(&s, "list_sources", map[string]any{"items": []any{}}, "")
			}
			got, err := CompactContext(s, s.Messages)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, m := range got {
				if m.ToolCallID == "round-0" {
					var r ToolResult
					_ = json.Unmarshal([]byte(m.Content), &r)
					found = strings.Contains(string(r.Data), "CRITICAL_END_FACT")
				}
			}
			if !found {
				t.Fatal("unprocessed evidence lost without context pressure")
			}
		})
	}
}

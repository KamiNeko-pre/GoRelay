package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"strings"
	"testing"
)

func TestProgramDiagnosticBudgetSurvivesModelPresentation(t *testing.T) {
	for _, name := range []string{"validate_program", "submit_acquisition"} {
		data, _ := json.Marshal(map[string]any{"candidate_id": "checked-candidate", "next_action": map[string]any{"tool": "submit_acquisition", "arguments": map[string]string{"candidate_id": "checked-candidate"}}, "result": map[string]any{"body": strings.Repeat("diagnostic source ", 600)}})
		out := modelObservationForRun(agentdata.Run{BudgetJSON: `{"loop_version":5}`}, ToolResult{CallID: "checked", Name: name, Data: data}, nil)
		if !strings.Contains(string(out.Data), "checked-candidate") || !strings.Contains(string(out.Data), "next_action") {
			t.Fatalf("owner's diagnostic budget discarded by presentation: %s", out.Data)
		}
	}
}

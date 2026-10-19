package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"testing"
)

func TestRunAssessmentSchemaUsesFrozenRequirementIDs(t *testing.T) {
	for _, fields := range [][]FieldRequirement{
		{{Name: "repository_name", Type: "string"}, {Name: "star_count", Type: "integer"}},
		{{Name: "product", Type: "string"}, {Name: "price", Type: "number"}},
	} {
		for _, requirements := range [][]TaskRequirement{nil, {{ID: "scope", Description: "cover scope"}, {ID: "comparison", Description: "compare evidence"}}} {
			spec := TaskSpec{RequiredFields: fields, Requirements: requirements}
			raw, _ := json.Marshal(spec)
			r := agentdata.Run{SpecJSON: string(raw)}
			for _, def := range runToolDefinitions(r) {
				if def.Function.Name != "update_task_progress" && def.Function.Name != "finish_task" {
					continue
				}
				var schema map[string]any
				if err := json.Unmarshal(def.Function.Parameters, &schema); err != nil {
					t.Fatal(err)
				}
				a := schema["properties"].(map[string]any)["requirements"].(map[string]any)
				if len(requirements) == 0 {
					if a["maxItems"] != float64(0) {
						t.Fatalf("%s exposes invented assessments for extraction-only task: %s", def.Function.Name, def.Function.Parameters)
					}
				} else {
					id := a["items"].(map[string]any)["properties"].(map[string]any)["id"].(map[string]any)
					ids, ok := id["enum"].([]any)
					if !ok || len(ids) != 2 || ids[0] != "scope" || ids[1] != "comparison" {
						t.Fatalf("%s does not bind frozen IDs: %s", def.Function.Name, def.Function.Parameters)
					}
				}
			}
			// The executable validator still rejects columns used as assessment IDs.
			if err := validateRequirementDelivery(spec, "partial", []RequirementResult{{ID: fields[0].Name, Status: "missing", Explanation: "missing"}}, nil); err == nil {
				t.Fatal("field name accepted as requirement ID")
			}
		}
	}
	// An unplanned task has no frozen contract yet; do not constrain its schema.
	for _, def := range runToolDefinitions(agentdata.Run{}) {
		if def.Function.Name == "update_task_progress" {
			var schema map[string]any
			if err := json.Unmarshal(def.Function.Parameters, &schema); err != nil {
				t.Fatal(err)
			}
			a := schema["properties"].(map[string]any)["requirements"].(map[string]any)
			if _, bound := a["maxItems"]; bound {
				t.Fatal("unplanned task falsely frozen to no requirements")
			}
			if _, bound := a["items"].(map[string]any)["properties"].(map[string]any)["id"].(map[string]any)["enum"]; bound {
				t.Fatal("unplanned task ID domain constrained")
			}
		}
	}
	for _, def := range AgentToolDefinitions() {
		if def.Function.Name == "update_task_progress" {
			var schema map[string]any
			json.Unmarshal(def.Function.Parameters, &schema)
			if _, bound := schema["properties"].(map[string]any)["requirements"].(map[string]any)["maxItems"]; bound {
				t.Fatal("run binding mutated shared definitions")
			}
		}
	}
}

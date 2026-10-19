package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"testing"
)

// The current protocol exposes reusable capabilities independently of whether
// this run has already validated a new program. Admission owns correctness;
// historic tool counts and prose byte ceilings do not define availability.
func TestPromptAndToolsRetainCrawlerExecutionContract(t *testing.T) {
	budget, _ := json.Marshal(BudgetSnapshot{LoopVersion: 5})
	run := agentdata.Run{Goal: "collect", BudgetJSON: string(budget)}
	prompt := runSystemPrompt(run)
	for _, required := range []string{"plan_task", "read_data", "next_read", "submit_extraction", "_evidence", "source_text", "http_get", "html_find_all"} {
		if !contains(prompt, required) {
			t.Fatalf("system prompt lost runtime contract %q", required)
		}
	}
	names := map[string]bool{}
	for _, definition := range AgentToolDefinitions() {
		name := definition.Function.Name
		if name == "" || names[name] || !json.Valid(definition.Function.Parameters) {
			t.Fatalf("invalid or duplicate tool contract: %q", name)
		}
		names[name] = true
	}
	for _, phase := range []string{"", "correcting"} {
		state := LoopState{Run: run, Spec: &TaskSpec{Goal: run.Goal, SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "title", Type: "string"}}}}
		state.HostProgress.Recovery.Phase = phase
		available := map[string]ToolDefinition{}
		for _, definition := range runToolDefinitionsForState(state) {
			available[definition.Function.Name] = definition
		}
		for _, required := range []string{"inspect_source", "read_data", "validate_program", "register_source", "submit_acquisition", "finish_task"} {
			if _, ok := available[required]; !ok {
				t.Fatalf("phase %q hides an available acquisition/reuse tool %s", phase, required)
			}
		}
		for _, knowledgeTool := range []string{"read_evidence", "search_knowledge", "reuse_knowledge", "read_source_content"} {
			if _, ok := available[knowledgeTool]; ok {
				t.Fatalf("crawler phase %q exposes downstream knowledge/prose tool %s", phase, knowledgeTool)
			}
		}
		var schema struct {
			Properties struct{ View struct{ Enum []string } }
		}
		if err := json.Unmarshal(available["read_data"].Function.Parameters, &schema); err != nil {
			t.Fatal(err)
		}
		views := map[string]bool{}
		for _, view := range schema.Properties.View.Enum {
			views[view] = true
		}
		if !views["dom"] || !views["json"] || !views["receipt"] || len(views) != 3 {
			t.Fatalf("crawler reader no longer paginates structure/receipts only: %v", views)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

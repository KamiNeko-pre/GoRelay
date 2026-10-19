package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"strings"
	"testing"
)

func TestE1ActionPromptMatchesAvailableTools(t *testing.T) {
	for _, version := range []int{5, 6} {
		for _, mode := range []string{"code", "per_tool"} {
			t.Run(mode+string(rune('0'+version)), func(t *testing.T) {
				b, _ := json.Marshal(map[string]any{"loop_version": version})
				in, _ := json.Marshal(CreateRunRequest{ActionMode: mode})
				r := agentdata.Run{BudgetJSON: string(b), InputJSON: string(in)}
				p := runSystemPrompt(r)
				names := map[string]bool{}
				for _, tool := range runToolDefinitions(r) {
					names[tool.Function.Name] = true
				}
				if mode == "per_tool" {
					if names["validate_program"] || names["register_source"] || strings.Contains(p, "register_source") || strings.Contains(p, "validate_program") {
						t.Fatal("per-tool prompt directs unavailable Lua publication")
					}
					if !strings.Contains(p, "submit_extraction") || !strings.Contains(p, "finish_task") {
						t.Fatal("missing completion path")
					}
				} else {
					if !names["validate_program"] || !strings.Contains(p, "register_source") {
						t.Fatal("code publication path removed")
					}
				}
			})
		}
	}
}

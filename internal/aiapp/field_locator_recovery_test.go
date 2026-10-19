package aiapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
)

func TestMissingLocatorDiagnosticSuppliesVerifiedRepair(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		value      any
		hint       bool
	}{
		{"table-field", `<table><tr><th>预算金额</th><td id="cell_dynamic_budget">17.50万元</td></tr></table>`, "17.50万元", true},
		{"json-field", `{"dataset":{"rights":{"license":"CC-BY-4.0"}}}`, "CC-BY-4.0", true},
		{"forged-value", `<td>17.50万元</td>`, "999万元", false},
		{"ambiguous-json", `{"first":"same","second":"same"}`, "same", false},
		{"ambiguous-dom", `<h2>same</h2><aside>same</aside>`, "same", false},
		{"prose", `<article><p>` + strings.Repeat("private article ", 30) + `</p></article>`, strings.TrimSpace(strings.Repeat("private article ", 30)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := "https://source.example/detail"
			source := acquisition.Response{Receipt: acquisition.Receipt{URL: url, Method: "GET", Status: 200, FetchedAt: time.Now()}, Body: tc.body}
			result := engine.ProgramResult{SourceEvidence: []acquisition.Response{source}}
			support := hostFieldSupport(result, url, map[string]any{"field": tc.value}, nil)["field"]
			var reported struct {
				Locator *fieldLocator `json:"suggested_locator"`
			}
			json.Unmarshal(mustJSON(support), &reported)
			if support.FullSource {
				// Complete text already has its existing host-owned proof; it must
				// not be turned into a model-readable locator repair preview.
				if tc.hint || reported.Locator != nil {
					t.Fatal("full-source boundary changed")
				}
				return
			}
			if support.Status != "unsupported" || (reported.Locator != nil) != tc.hint {
				t.Fatalf("diagnostic has no verified repair or invented one: %+v", support)
			}
			if !tc.hint {
				return
			}
			actual, _, err := evidenceValue(source, *reported.Locator)
			if err != nil || canonicalDeliveryValue(actual) != canonicalDeliveryValue(tc.value) {
				t.Fatal("suggested repair is not source-verified", actual, err)
			}
			rows := []Record{{Fields: map[string]any{"field": tc.value}, Provenance: Provenance{URL: url, FieldSupport: map[string]FieldSupport{"field": support}}}}
			err = requireSupportedFields(TaskSpec{RequireFieldSupport: true}, rows)
			failure := toolFailureReceipt(ToolCall{Function: FunctionCall{Name: "validate_program"}}, ToolResult{}, err)
			raw := string(mustJSON(failure))
			if err == nil || !strings.Contains(raw, "suggested_locator") || !strings.Contains(raw, "_evidence") {
				t.Fatal("failure discarded actionable locator repair", raw)
			}
			var locator map[string]any
			json.Unmarshal(mustJSON(reported.Locator), &locator)
			fixed := hostFieldSupport(result, url, map[string]any{"field": tc.value}, map[string]any{"field": locator})["field"]
			if fixed.Status != "supported" {
				t.Fatal("applying verified repair still fails", fixed)
			}
			other := hostFieldSupport(result, "https://source.example/other", map[string]any{"field": tc.value}, nil)["field"]
			var wrongSource struct {
				Locator *fieldLocator `json:"suggested_locator"`
			}
			json.Unmarshal(mustJSON(other), &wrongSource)
			if wrongSource.Locator != nil || other.Status == "supported" {
				t.Fatal("another source borrowed a locator")
			}
		})
	}
}

func TestLuaAndEvidenceShareCSSSelectorContract(t *testing.T) {
	runner := actualRunnerForChain(t)
	body := `<main><h2>Project title</h2><table><tr><td id="cell_random_project_id">CG-42</td><td class="value">17万元</td></tr></table></main>`
	for _, selector := range []string{`td[id$='_project_id']`, `table td:nth-of-type(2)`, `main > h2`} {
		url := "https://source.example/detail"
		result, err := runner.Execute(context.Background(), RunnerRequest{Program: engine.ProgramRequest{Code: `local v=assert(html_find(PARAMS.html,PARAMS.selector)); return true,{items={{value=v,source_url=PARAMS.url,_evidence={value={selector=PARAMS.selector,index=0}}}}}`, Params: map[string]any{"html": body, "selector": selector, "url": url}}, Limits: engine.DefaultProgramLimits()})
		if err != nil {
			t.Fatal(selector, err)
		}
		receipt := acquisition.Receipt{URL: url, Method: "GET", Status: 200, FetchedAt: time.Now()}
		result.Requests, result.SourceEvidence = []acquisition.Receipt{receipt}, []acquisition.Response{{Receipt: receipt, Body: body}}
		rows, err := BindRecords(result, RecordBinding{SourceID: "css-contract", DataVersion: "v1", TaskID: 1, SchemaVersion: "v1"})
		if err != nil {
			t.Fatal(err)
		}
		if err := requireSupportedFields(TaskSpec{RequireFieldSupport: true}, rows); err != nil {
			t.Fatal("Lua and evidence selector grammars diverged", selector, err)
		}
	}
	prompt := runSystemPrompt(agentdata.Run{BudgetJSON: `{"loop_version":5}`})
	for _, contract := range []string{"Cascadia", "Lua 5.1", "gopher-lua v1.1.2"} {
		if !strings.Contains(prompt, contract) {
			t.Fatal("model still must guess its runtime/selector contract", contract)
		}
	}
}

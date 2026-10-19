package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/engine"
)

func TestCrawlerProgramBudgetDoesNotSilentlyShrinkAtValidation(t *testing.T) {
	for _, mode := range []string{"probe", "acquisition"} {
		t.Run(mode, func(t *testing.T) {
			s, run, _ := storedHTMLReader(t, `<h2>source</h2>`)
			spec := TaskSpec{Goal: run.Goal, SchemaVersion: "v1", ContractVersion: "v2", RequiredFields: []FieldRequirement{{Name: "value", Type: "string"}, {Name: "source_url", Type: "string"}}, RequireFieldSupport: true}
			run.SpecJSON = string(mustJSON(spec))
			if err := s.Data.DB.Model(&run).Update("spec_json", run.SpecJSON).Error; err != nil {
				t.Fatal(err)
			}
			base := "https://fixture.example/"
			entries := []acquisition.ReplayEntry{}
			for i := 1; i <= 9; i++ {
				entries = append(entries, acquisition.ReplayEntry{Method: "GET", URL: fmt.Sprintf("%s%d", base, i), Status: 200, Body: `<h2>actual value</h2><p>` + strings.Repeat("large source ", 12000) + `</p>`, FetchedAt: time.Now()})
			}
			var received RunnerRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&received)
				// This is the same executable tool path/VM, not a scripted result.
				replay := acquisition.NewReplay(entries)
				limits := received.Limits
				if limits.Timeout <= 0 {
					limits.Timeout = time.Second
				}
				result, err := engine.RunProgram(r.Context(), received.Program, limits, replay.Fetch)
				out := RunnerResponse{Result: &result}
				if err != nil {
					out.Error = &RunnerError{Code: "budget_exceeded", Message: err.Error()}
				}
				json.NewEncoder(w).Encode(out)
			}))
			defer server.Close()
			s.Runner, _ = NewRunnerClient(server.URL, "fixture")
			code := `local last,body;for i=1,9 do last=PARAMS.base..i;body=assert(http_get(last)) end;return true,{items={{value=assert(html_find(body,"h2")),source_url=last,_evidence={value={selector="h2",index=0}}}}}`
			fields := []string{"value", "source_url"}
			call := ToolCall{ID: "valid-large-program", Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: string(mustJSON(map[string]any{"kind": "adapter", "mode": mode, "source_id": "generic-resource-test", "code": code, "url": base + "1", "params": map[string]any{"base": base}, "produced_fields": fields}))}}
			out, err := s.trial(context.Background(), run, call)
			if err != nil {
				t.Fatal("valid multi-page program was shrunk to 8 requests/1 MiB", err)
			}
			if received.Limits.MaxRequests <= 8 || received.Limits.MaxBytes <= 1<<20 || received.Limits.Timeout < 5*time.Minute || out.HTTP == nil || out.HTTP.Requests != 9 {
				t.Fatal("execution did not receive the frozen host resource/time budget", received.Limits, out.HTTP)
			}
			if _, exists := func() (string, bool) {
				var b map[string]json.RawMessage
				json.Unmarshal([]byte(run.BudgetJSON), &b)
				v, ok := b["program_limits"]
				return string(v), ok
			}(); !exists {
				t.Fatal("execution policy was not frozen with task")
			}
		})
	}
}

func TestGeneratedLuaPromptMatchesActualRuntimeGlobals(t *testing.T) {
	runner := actualRunnerForChain(t)
	result, err := runner.Execute(context.Background(), RunnerRequest{Program: engine.ProgramRequest{Code: `return true,{items={{version=_VERSION,limited=type(http_get_limited),referer=type(http_get_referer),get=type(http_get),head=type(http_head),source_url="https://fixture.example/"}}}`}, Limits: engine.DefaultProgramLimits()})
	if err != nil {
		t.Fatal(err)
	}
	item := result.Collection["items"].([]any)[0].(map[string]any)
	if item["version"] != "Lua 5.1" || item["get"] != "function" || item["head"] != "function" {
		t.Fatal("runtime contract changed", item)
	}
	for _, api := range []struct{ name, field string }{{"http_get_limited", "limited"}, {"http_get_referer", "referer"}} {
		if strings.Contains(luaSandboxInstructions, api.name+"(") && item[api.field] != "function" {
			t.Fatal("prompt advertises a missing runtime function", api.name)
		}
	}
}

func TestRunnerResourceFailureExplainsActualBudgetAndBatchRepair(t *testing.T) {
	for _, code := range []string{"budget_exceeded", "timeout"} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(RunnerResponse{Error: &RunnerError{Code: code, Message: "resource boundary"}, Result: &engine.ProgramResult{Stats: acquisition.Stats{Requests: 2, Bytes: 53}}})
			}))
			defer server.Close()
			client, _ := NewRunnerClient(server.URL, "fixture")
			limits := engine.DefaultProgramLimits()
			limits.MaxRequests = 2
			limits.MaxBytes = 53
			_, err := client.Execute(context.Background(), RunnerRequest{Limits: limits})
			failure := toolFailureReceipt(ToolCall{ID: "resource-error", Function: FunctionCall{Name: "validate_program"}}, ToolResult{}, err)
			raw := string(mustJSON(failure))
			for _, information := range []string{"limits", "observed_io", "max_requests", "53", "partition", "params"} {
				if !strings.Contains(raw, information) {
					t.Fatal("resource error tells model to guess program edits", information, raw)
				}
			}
		})
	}
}

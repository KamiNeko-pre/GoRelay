package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
)

func TestLuaAcquisitionEvidenceContractReachesFirstModelHTTP(t *testing.T) {
	for _, fields := range [][]string{{"title", "attachment_path", "source_url"}, {"publisher", "resource_path", "source_url"}} {
		t.Run(fields[0], func(t *testing.T) {
			spec := TaskSpec{RequireFieldSupport: true}
			for _, field := range fields {
				spec.RequiredFields = append(spec.RequiredFields, FieldRequirement{Name: field, Type: "string"})
			}
			run := agentdata.Run{BudgetJSON: `{"loop_version":5}`, SpecJSON: string(mustJSON(spec))}
			calls := 0
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				var req ChatRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				found := false
				for _, tool := range req.Tools {
					if tool.Function.Name != "validate_program" {
						continue
					}
					found = true
					var schema struct {
						Properties map[string]struct {
							Description string `json:"description"`
						} `json:"properties"`
					}
					json.Unmarshal(tool.Function.Parameters, &schema)
					contract := schema.Properties["code"].Description
					for _, fragment := range []string{"mode=acquisition", "mode=probe", "_evidence", "return true", "source_url", "selector", "attribute", "json_pointer", "first acquisition", "does not create", "require_field_support=true"} {
						if !strings.Contains(contract, fragment) {
							t.Errorf("first decision's Lua code contract lacks %q", fragment)
						}
					}
					for _, field := range fields {
						if !strings.Contains(contract, field) {
							t.Errorf("frozen field %q is absent from the Lua output contract", field)
						}
					}
				}
				if !found {
					t.Error("validate_program missing on first model request")
				}
				auditSave(t, "first-lua-evidence-contract-"+fields[0], req)
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant"}, "finish_reason": "stop"}}})
			}))
			defer endpoint.Close()
			provider, _ := NewModelProvider(ProviderConfig{BaseURL: endpoint.URL, APIKey: "test-only", Model: "contract-transport"})
			_, err := provider.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "system", Content: runSystemPrompt(run)}, {Role: "user", Content: "collect the frozen fields"}}, Tools: runToolDefinitions(run)})
			if err != nil || calls != 1 {
				t.Fatal("first request was not verified", err, calls)
			}
		})
	}
}

func TestEvidenceFailureHeadlineAndCorrectionNameAllFields(t *testing.T) {
	for _, names := range [][]string{
		{"attachment_path", "budget", "buyer", "contact", "deadline", "project_id", "region", "title"},
		{"publisher", "resource_path"},
	} {
		t.Run(names[0], func(t *testing.T) {
			s := efficiencyState()
			records := []Record{}
			for i := 0; i < 2; i++ {
				record := Record{Fields: map[string]any{"source_url": fmt.Sprintf("https://source.example/detail/%d", i)}}
				record.Provenance.URL = record.Fields["source_url"].(string)
				record.Provenance.FieldSupport = map[string]FieldSupport{"source_url": {Status: "supported", CheckerVersion: "host-locator-v1", EvidenceRefs: []string{record.Provenance.URL + "#successful-get"}}}
				for _, name := range names {
					record.Fields[name] = "source value"
					record.Provenance.FieldSupport[name] = FieldSupport{Status: "unsupported", Reason: "missing _evidence." + name + " locator"}
				}
				records = append(records, record)
			}
			cause := requireSupportedFields(TaskSpec{RequireFieldSupport: true}, records)
			out := toolFailureReceipt(ToolCall{ID: "missing-proof", Function: FunctionCall{Name: "validate_program"}}, ToolResult{}, cause)
			if out.Failure == nil {
				t.Fatal("missing evidence was accepted")
			}
			s.Operations = []agentdata.Operation{{RunID: s.Run.ID, Key: "tool:" + out.CallID, Kind: out.Name, Stage: "failed", ResultJSON: string(mustJSON(out))}}
			brief := strings.Join(failingAndExpected(s), " ")
			for _, name := range names {
				if !strings.Contains(out.Error, name) || !strings.Contains(brief, name) {
					t.Errorf("multi-field failure presents only a first example instead of %q: headline=%s brief=%s", name, out.Error, brief)
				}
			}
			var detail struct {
				Fields      []string `json:"fields"`
				FieldCount  int      `json:"unsupported_field_count"`
				RecordCount int      `json:"unsupported_record_count"`
			}
			json.Unmarshal(mustJSON(out.Failure.Details), &detail)
			want := append([]string{}, names...)
			sort.Strings(want)
			if strings.Join(detail.Fields, ",") != strings.Join(want, ",") || detail.FieldCount != len(names) || detail.RecordCount != 2 {
				t.Error("field/record summary is missing or disagrees with actual failed checks", detail)
			}
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req ChatRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				var received ToolResult
				if len(req.Messages) != 2 || json.Unmarshal([]byte(req.Messages[1].Content), &received) != nil {
					t.Error("actual model request lost the failure envelope")
				}
				for _, field := range names {
					if !strings.Contains(received.Error, field) {
						t.Errorf("model HTTP lost batch field %q", field)
					}
				}
				if received.Failure == nil || len(received.Failure.Recovery.Steps) != 3 {
					t.Error("model HTTP lost the ordered correction steps")
				}
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant"}, "finish_reason": "stop"}}})
			}))
			defer endpoint.Close()
			provider, _ := NewModelProvider(ProviderConfig{BaseURL: endpoint.URL, APIKey: "test-only", Model: "failure-transport"})
			visible := modelObservationForRun(s.Run, out, nil)
			if _, err := provider.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: out.CallID, Type: "function", Function: FunctionCall{Name: out.Name, Arguments: `{}`}}}}, {Role: "tool", ToolCallID: out.CallID, Content: string(mustJSON(visible))}}}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEvidenceContractPreservesValidatedAndOptionalProof(t *testing.T) {
	url := "https://source.example/detail"
	receipt := acquisition.Receipt{URL: url, Method: "GET", Status: 200, FetchedAt: time.Now()}
	result := engine.ProgramResult{SourceEvidence: []acquisition.Response{{Receipt: receipt, Body: `<main><h2>Observed title</h2><a href="../files/data.csv?edition=2026&amp;lang=zh">Download</a></main>`}}}
	fields := map[string]any{"title": "Observed title", "resource_path": "../files/data.csv?edition=2026&lang=zh", "source_url": url}
	locators := map[string]any{"title": map[string]any{"selector": "h2", "index": 0}, "resource_path": map[string]any{"selector": "a", "index": 0, "attribute": "href"}}
	record := Record{Fields: fields, Provenance: Provenance{URL: url, FieldSupport: hostFieldSupport(result, url, fields, locators)}}
	if err := requireSupportedFields(TaskSpec{RequireFieldSupport: true}, []Record{record}); err != nil {
		t.Fatal("correct value/locator pair rejected", err)
	}
	missing := record
	missing.Provenance.FieldSupport = hostFieldSupport(result, url, fields, nil)
	if err := requireSupportedFields(TaskSpec{}, []Record{missing}); err != nil {
		t.Fatal("optional proof became compulsory", err)
	}
	if err := requireSupportedFields(TaskSpec{RequireFieldSupport: true}, []Record{missing}); err == nil {
		t.Fatal("mandatory proof was weakened")
	}
	full, err := acquisition.SourceText(result.SourceEvidence[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	fullFields := map[string]any{"body": full, "source_url": url}
	fullRecord := Record{Fields: fullFields, Provenance: Provenance{URL: url, FieldSupport: hostFieldSupport(result, url, fullFields, nil)}}
	if err := requireSupportedFields(TaskSpec{RequireFieldSupport: true}, []Record{fullRecord}); err != nil || !fullRecord.Provenance.FieldSupport["body"].FullSource {
		t.Fatal("host full-source proof was changed", err)
	}
}

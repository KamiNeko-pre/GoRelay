package aiapp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
)

func TestJSONStructuralDefaultsHaveOneBusinessIdentity(t *testing.T) {
	page := inspectResponse(acquisition.Response{Body: `{"collections":[{"href":"/one"},{"href":"/two"}]}`, Receipt: acquisition.Receipt{Method: "GET", URL: "https://source.example/", Status: 200, FetchedAt: time.Now()}}, acquisition.Stats{})
	out, _ := toolData(ToolCall{ID: "json-source", Function: FunctionCall{Name: "inspect_source"}}, page)
	ops := []agentdata.Operation{{Kind: "inspect_source", Key: "tool:json-source", Stage: "succeeded", ResultJSON: string(mustJSON(out))}}
	a := FunctionCall{Name: "read_data", Arguments: `{"handle":"json-source","view":"json","pointer":"/collections"}`}
	b := FunctionCall{Name: "read_data", Arguments: `{"handle":"json-source","view":"json","pointer":"/collections","limit":64,"view_version":"json-structure-v1"}`}
	if businessToolInput(a, ops) != businessToolInput(b, ops) {
		t.Fatal("explicit default/version bypasses structural read identity")
	}
	b.Arguments = `{"handle":"json-source","view":"json","pointer":"/collections/1"}`
	if businessToolInput(a, ops) == businessToolInput(b, ops) {
		t.Fatal("different structural selectors collapsed into one action")
	}
}

func TestConditionalSchemaValidationIsGeneric(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string"},"ids":{"type":"array","items":{"type":"integer"}}},"if":{"required":["kind"],"properties":{"kind":{"enum":["batch"]}}},"then":{"required":["ids"],"properties":{"ids":{"type":"array","minItems":2}}},"additionalProperties":false}`)
	defs := []ToolDefinition{{Function: FunctionDefinition{Name: "independent_schema_case", Parameters: schema}}}
	for _, tc := range []struct {
		args string
		bad  bool
	}{{`{"kind":"batch"}`, true}, {`{"kind":"batch","ids":[1]}`, true}, {`{"kind":"batch","ids":[1,2]}`, false}, {`{"kind":"single"}`, false}, {`{}`, false}} {
		err := requiredToolArguments(FunctionCall{Name: "independent_schema_case", Arguments: tc.args}, defs)
		if (err != nil) != tc.bad {
			t.Error("conditional validation depends on a particular tool/mode", tc.args, err)
		}
	}
}

func TestJSONStructurePagesAllEntriesWithoutReadingProse(t *testing.T) {
	rows := []map[string]any{}
	for i := 0; i < 5; i++ {
		rows = append(rows, map[string]any{"name": fmt.Sprint("collection-", i), "href": fmt.Sprintf("/collections/%d", i)})
	}
	body := string(mustJSON(map[string]any{"collections": rows, "description": strings.Repeat("PRIVATE ARTICLE TEXT ", 100)}))
	s, run, _ := storedHTMLReader(t, body)
	run.BudgetJSON = `{"loop_version":5}`
	a := dataReadArgs{Handle: "source", View: "json", Pointer: "/collections", Limit: 2}
	seen := 0
	for page := 0; page < 5; page++ {
		out, err := s.readData(context.Background(), run, ToolCall{ID: fmt.Sprint(page), Function: FunctionCall{Name: "read_data", Arguments: string(mustJSON(a))}})
		if err != nil {
			t.Fatal("JSON structure entries cannot continue", err)
		}
		var data struct {
			Nodes []struct {
				Pointer string         `json:"json_pointer"`
				Sample  map[string]any `json:"sample"`
			} `json:"nodes"`
			Next *dataReadArgs `json:"next_read"`
		}
		json.Unmarshal(out.Data, &data)
		for _, node := range data.Nodes {
			if node.Pointer != fmt.Sprintf("/collections/%d", seen) || node.Sample["href"] != fmt.Sprintf("/collections/%d", seen) {
				t.Fatal("JSON path/short navigation value lost", node)
			}
			seen++
		}
		if data.Next == nil {
			break
		}
		a = *data.Next
	}
	if seen != len(rows) {
		t.Fatal("only first JSON array element is accessible", seen)
	}
	for _, tc := range []struct{ pointer, sample string }{{"/collections/1/href", "/collections/1"}, {"/description", ""}} {
		out, err := s.readData(context.Background(), run, ToolCall{Function: FunctionCall{Name: "read_data", Arguments: string(mustJSON(dataReadArgs{Handle: "source", View: "json", Pointer: tc.pointer}))}})
		if err != nil {
			t.Fatal("short JSON field/long-field metadata rejected as prose", err)
		}
		if tc.sample != "" && !strings.Contains(string(out.Data), tc.sample) {
			t.Fatal("observed href is unavailable", string(out.Data))
		}
		if strings.Contains(string(out.Data), "PRIVATE ARTICLE TEXT") {
			t.Fatal("article entered structural field projection")
		}
	}
	if _, err := s.readData(context.Background(), run, ToolCall{Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"json","pointer":"/description","offset":1}`}}); err == nil {
		t.Fatal("JSON prose gained text pagination")
	}
}

func TestProbeSourceStructureReplacesOpaqueDumpLoop(t *testing.T) {
	body := `{"dataset":{"publisher_ref":"org_1","distribution_refs":["json_1","csv_1"]},"organizations":{"org_1":{"name":"Observed public institution"}},"distributions":{"csv_1":{"format":"CSV","href":"../data.csv?edition=final&lang=zh"}},"description":"` + strings.Repeat("PRIVATE ARTICLE TEXT ", 100) + `"}`
	probeFeedbackProxy(t, body)
	s, run, _ := storedHTMLReader(t, body)
	s.Runner = actualRunnerForChain(t)
	run.BudgetJSON = `{"loop_version":5}`
	run.SpecJSON = `{"goal":"collect publisher","contract_version":"v2","schema_version":"v1","required_fields":[{"name":"publisher","type":"string"},{"name":"source_url","type":"string"}],"join_keys":["publisher"],"require_field_support":true,"require_published_capability":true}`
	args := trialArgs{Kind: "composition", Mode: "probe", SourceID: "structure-probe", Purpose: "inspect structured API", URL: "http://8.8.8.8/detail", ProducedFields: []string{"kind", "value"}, Code: `local b=assert(http_get(TARGET_URL));return true,{items={{kind="detail",value=b}}}`}
	call := ToolCall{ID: "opaque-json", Function: FunctionCall{Name: "validate_program", Arguments: string(mustJSON(args))}}
	out, err := s.trial(context.Background(), run, call)
	if err != nil {
		t.Fatal(err)
	}
	visible := modelObservationForRun(run, out, nil)
	var data struct {
		Sources     []json.RawMessage `json:"observed_sources"`
		Truncated   bool              `json:"preview_truncated"`
		Next        map[string]any    `json:"next_action"`
		Diagnostics []any             `json:"field_diagnostics"`
		Step        string            `json:"next_step"`
	}
	json.Unmarshal(visible.Data, &data)
	if len(data.Sources) != 1 || !strings.Contains(string(data.Sources[0]), "Observed public institution") || !strings.Contains(string(data.Sources[0]), "csv_1") {
		t.Error("actual runner GET structure was hidden behind a truncated print", string(visible.Data))
	}
	if strings.Contains(string(visible.Data), "PRIVATE ARTICLE TEXT") || strings.Contains(string(visible.Data), "source_evidence") {
		t.Error("raw source crossed model boundary")
	}
	if !data.Truncated || data.Next["tool"] != "validate_program" || strings.Contains(data.Step, "pointer=/items") || len(data.Diagnostics) != 0 {
		t.Error("generic exploration is misreported as incomplete delivery or given an unusable next step", data)
	}
	state := efficiencyState()
	state.Run = run
	op := agentdata.Operation{ID: 1, RunID: run.ID, Key: "tool:" + out.CallID, Kind: out.Name, Stage: "succeeded", ResultJSON: string(mustJSON(out))}
	if len(correctionEvidenceKeys(state, op)) == 0 {
		t.Error("new host GET structure is not correction evidence")
	}
	args.Code = `return true,{items={{kind="detail",value="invented",source_url=TARGET_URL}}}`
	call.ID = "printed-only"
	call.Function.Arguments = string(mustJSON(args))
	printed, err := s.trial(context.Background(), run, call)
	if err != nil {
		t.Fatal(err)
	}
	op.ResultJSON = string(mustJSON(printed))
	if len(correctionEvidenceKeys(state, op)) != 0 {
		t.Error("model-only printed data manufactured source progress")
	}
}

func TestProbeConditionalDeclarationMatchesPublishedSchema(t *testing.T) {
	run := agentdata.Run{BudgetJSON: `{"loop_version":5}`}
	base := map[string]any{"code": "return true,{items={}}", "kind": "composition", "source_id": "contract", "purpose": "inspect", "url": "https://source.example/"}
	for _, mode := range []string{"probe", "acquisition", ""} {
		for _, fields := range []any{nil, []string{}, []string{"link"}} {
			a := map[string]any{}
			for k, v := range base {
				a[k] = v
			}
			if mode != "" {
				a["mode"] = mode
			}
			if fields != nil {
				a["produced_fields"] = fields
			}
			err := requiredToolArguments(FunctionCall{Name: "validate_program", Arguments: string(mustJSON(a))}, runToolDefinitions(run))
			wantErr := mode == "probe" && (fields == nil || len(fields.([]string)) == 0)
			if (err != nil) != wantErr {
				t.Errorf("mode=%q fields=%v conditional requiredness disagrees with runtime: %v", mode, fields, err)
			}
		}
	}
	for _, d := range runToolDefinitions(run) {
		if d.Function.Name == "validate_program" {
			var schema map[string]any
			json.Unmarshal(d.Function.Parameters, &schema)
			if schema["if"] == nil || schema["then"] == nil {
				t.Error("probe declaration condition is absent from offered schema")
			}
			for _, text := range []string{"_evidence", "json_pointer", "produced_fields"} {
				if !strings.Contains(d.Function.Description, text) {
					t.Error("top-level tool contract hides critical syntax", text)
				}
			}
		}
	}
	if !strings.Contains(runSystemPrompt(run), "json_pointer") {
		t.Error("previously working system-level JSON evidence syntax was lost")
	}
}

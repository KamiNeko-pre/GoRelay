package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"strings"
	"testing"
	"time"
)

func TestPerToolBaselineUsesPersistedReceiptsAndSameDeliveryContract(t *testing.T) {
	t.Setenv("GORELAY_ACTION_MODE", "per_tool")
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := TaskSpec{Goal: "collect", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "name", Type: "string"}, {Name: "version", Type: "string"}}, JoinKeys: []string{"name"}}
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "baseline", Goal: spec.Goal, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	observation, _ := toolData(ToolCall{ID: "page", Function: FunctionCall{Name: "inspect_source"}}, sourceInspection{Source: acquisition.Response{Receipt: acquisition.Receipt{Method: "GET", URL: "https://fixture.example/releases", Status: 200, FetchedAt: time.Now()}, Body: strings.Repeat("x", 13000)}})
	raw, _ := json.Marshal(observation)
	if err = s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:page", Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
		t.Fatal(err)
	}
	slice := ToolCall{ID: "slice", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"page","view":"content","pointer":"/text","offset":12000,"view_version":"plain-text-v1"}`}}
	part, err := s.executeDurableTool(ctx, r, slice)
	if err != nil || !strings.Contains(string(part.Data), `"text":"`+strings.Repeat("x", 1000)+`"`) {
		t.Fatalf("full source is not readable without refetch: %s %v", part.Data, err)
	}
	call := ToolCall{ID: "extract", Type: "function", Function: FunctionCall{Name: "submit_extraction", Arguments: `{"inspection_call_id":"page","items":[{"name":"Alpha","version":"1.0","source_url":"https://fixture.example/releases"}]}`}}
	first, err := s.executeDurableTool(ctx, r, call)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.executeDurableTool(ctx, r, call); err != nil {
		t.Fatal(err)
	}
	children, err := s.ChildResults(ctx, r.ID)
	if err != nil || len(children) != 1 || len(children[0].Records) != 1 {
		t.Fatal("duplicate or missing extraction result")
	}
	result, err := s.Evaluate(ctx, r)
	if err != nil || !result.Coverage.Satisfied {
		t.Fatalf("same frozen goal not satisfied: %+v %v", result, err)
	}
	if len(first.Data) == 0 || children[0].Records[0].Provenance.DataVersion == "" {
		t.Fatal("receipt/source identity missing")
	}
	var outboxes int64
	s.Data.DB.Table("task_outboxes").Count(&outboxes)
	if outboxes != 0 {
		t.Fatal("already extracted facts were re-executed by Worker")
	}
	bad := call
	bad.ID = "forged"
	bad.Function.Arguments = strings.ReplaceAll(call.Function.Arguments, "https://fixture.example/releases", "https://other.example/invented")
	if _, err = s.executeDurableTool(ctx, r, bad); err == nil {
		t.Fatal("unobserved URL accepted")
	}
	bad.ID = "wrong-type"
	bad.Function.Arguments = strings.ReplaceAll(call.Function.Arguments, `"version":"1.0"`, `"version":1`)
	if _, err = s.executeDurableTool(ctx, r, bad); err == nil {
		t.Fatal("wrong field type accepted")
	}
	var tasks, artifacts int64
	s.Data.DB.Table("crawl_tasks").Count(&tasks)
	s.Data.DB.Table("program_artifacts").Count(&artifacts)
	if tasks != 1 || artifacts != 1 {
		t.Fatalf("failed extraction committed partial local mutations: tasks=%d artifacts=%d", tasks, artifacts)
	}
	ops, err := s.Data.Operations(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	failures := 0
	for _, op := range ops {
		if op.Kind == "submit_extraction" && op.Stage == "failed" {
			failures++
		}
	}
	if failures != 2 {
		t.Fatal("failed tool observations were lost with rolled-back mutations")
	}
	for _, def := range runToolDefinitions(r) {
		if def.Function.Name == "validate_program" {
			t.Fatal("baseline still exposes code actions")
		}
	}
	t.Setenv("GORELAY_ACTION_MODE", "code")
	codeRun, err := s.Create(ctx, CreateRunRequest{RequestKey: "code", Goal: "goal"})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, def := range runToolDefinitions(codeRun) {
		names[def.Function.Name] = true
	}
	if !names["submit_extraction"] || !names["validate_program"] {
		t.Fatal("main Agent cannot choose direct tools or Code as Action")
	}
	if actionMode(r) != "per_tool" {
		t.Fatal("configuration change altered a frozen Run mode")
	}
}

func TestSourceContentSelectsPersistedDOMWithoutRefetch(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "selector-read", Goal: "inspect structured records"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for n, body := range []string{
		`<html><head><script>` + strings.Repeat("header", 4000) + `</script></head><body><div id="list"><a class="row">Alpha 12</a><a class="row">Beta 18</a></div></body></html>`,
		`<html><body><table><tr><td>First 1.2</td></tr><tr><td>Second 2.3</td></tr></table></body></html>`,
	} {
		id := []string{"links", "table"}[n]
		selector := []string{`div[id="list"] > a`, "table tr"}[n]
		observation, _ := toolData(ToolCall{ID: id, Function: FunctionCall{Name: "inspect_source"}}, sourceInspection{Source: acquisition.Response{Receipt: acquisition.Receipt{Method: "GET", URL: "https://fixture.example/" + id, Status: 200, FetchedAt: time.Now()}, Body: body}})
		raw, _ := json.Marshal(observation)
		if err := s.Data.DB.Create(&agentdata.Operation{RunID: r.ID, Key: "tool:" + id, Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}).Error; err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]any{"inspection_call_id": id, "selector": selector, "offset": 0})
		call := ToolCall{ID: "read-" + id, Function: FunctionCall{Name: "read_source_content", Arguments: string(args)}}
		result, err := s.readSourceContent(ctx, r, call)
		var data struct {
			Text     string `json:"text"`
			Matches  int    `json:"matches"`
			Requests int    `json:"new_http_requests"`
		}
		_ = json.Unmarshal(result.Data, &data)
		if err != nil || data.Matches != 2 || data.Requests != 0 || strings.Contains(data.Text, "header") || data.Text == "" {
			t.Fatalf("structured read failed: %+v %v", data, err)
		}
		args, _ = json.Marshal(map[string]any{"inspection_call_id": id, "selector": ".absent", "offset": 0})
		call.Function.Arguments = string(args)
		if _, err := s.readSourceContent(ctx, r, call); err == nil {
			t.Fatal("missing selector returned fabricated or entire content")
		}
	}
}

package aiapp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
)

func TestCrawlerCannotReadProseThroughAnyViewOrReceipt(t *testing.T) {
	secret := strings.Repeat("正文不应交给爬虫模型分析。", 300)
	s, r, raw := storedHTMLReader(t, `<article><h2>Public title</h2><div>`+secret+`</div></article>`)
	ctx := context.Background()
	r, err := s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []string{`{"handle":"source","view":"content","selector":"article"}`, `{"handle":"source","view":"json","pointer":"/source/body"}`, `{"handle":"source","view":"receipt","pointer":"/source/body"}`} {
		out, err := s.readData(ctx, r, ToolCall{ID: "forbidden", Type: "function", Function: FunctionCall{Name: "read_data", Arguments: args}})
		if err == nil {
			t.Errorf("crawler accepted prose route: %s => %s", args, out.Data)
		}
	}
	var source ToolResult
	json.Unmarshal([]byte(raw), &source)
	projected := modelObservationForRun(r, source, nil)
	if strings.Contains(string(projected.Data), "正文不应交给爬虫模型分析。正文不应交给爬虫模型分析。") || strings.Contains(string(projected.Data), `"view":"content"`) {
		t.Errorf("inspection published prose or a prose read route: %s", projected.Data)
	}
	for _, view := range []string{"dom", "receipt"} {
		out, err := s.readData(ctx, r, ToolCall{ID: "structure", Function: FunctionCall{Name: "read_data", Arguments: `{"handle":"source","view":"` + view + `"}`}})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out.Data), secret) {
			t.Fatal("structure/metadata leaked complete prose")
		}
	}
	fn := FunctionCall{Name: "inspect_source", Arguments: `{"url":"https://fixture.example/docs"}`}
	if err := s.Data.DB.Model(&agentdata.Operation{}).Where("run_id = ? AND `key` = ?", r.ID, "tool:source").Update("input_hash", canonicalToolInput(fn)).Error; err != nil {
		t.Fatal(err)
	}
	duplicate, err := s.executeDurableToolWithVisibility(ctx, r, ToolCall{ID: "duplicate-fetch", Type: "function", Function: fn}, visibleReceipts{"source": true})
	if err == nil {
		t.Fatal("expected duplicate refusal")
	}
	projected = modelObservationForRun(r, duplicate, nil)
	encoded, _ := json.Marshal(projected)
	if strings.Contains(string(encoded), secret) {
		t.Fatal("duplicate error exposed the SQL original article body")
	}
	ops, err := s.Data.Operations(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := contextFacts(LoopState{Run: r, Budget: BudgetSnapshot{LoopVersion: 5}, Operations: ops})
	if err != nil || strings.Contains(string(facts), secret) {
		t.Fatal("unresolved failure memory reintroduced the original article after compaction", err)
	}
}

func TestCrawlerMenuKeepsItsCompleteDeliveryPathWithoutRAG(t *testing.T) {
	s := &Service{}
	state := decisionState()
	state.Budget.LoopVersion = 5
	state.Run.BudgetJSON = `{"loop_version":5}`
	have := map[string]bool{}
	for _, d := range s.configuredToolDefinitionsForState(state) {
		have[d.Function.Name] = true
	}
	for _, name := range []string{"inspect_source", "read_data", "list_sources", "validate_program", "register_source", "submit_acquisition", "get_task_results", "finish_task"} {
		if !have[name] {
			t.Errorf("missing crawler delivery capability %s", name)
		}
	}
	for _, name := range []string{"search_knowledge", "read_evidence", "reuse_knowledge", "read_observation", "read_source_content"} {
		if have[name] {
			t.Errorf("crawler exposes a second RAG/prose loop: %s", name)
		}
	}
}

func TestCrawlerProbeCannotSmuggleUndeclaredProse(t *testing.T) {
	secret := strings.Repeat("Complete article prose belongs in stored records. ", 40)
	out, _ := toolData(ToolCall{ID: "probe", Function: FunctionCall{Name: "validate_program"}}, map[string]any{"id": "candidate", "items": []any{map[string]any{"unexpected_field": secret, "title": "A useful short title"}}, "metadata": map[string]any{"unrequested": secret}, "next_action": map[string]any{"tool": "register_source", "arguments": map[string]any{"candidate_id": "candidate"}}})
	projected := crawlerObservation(out, nil)
	if strings.Contains(string(projected.Data), "Complete article prose") {
		t.Fatal("undeclared program output leaked article prose")
	}
	for _, value := range []string{"candidate", "register_source", "A useful short title"} {
		if !strings.Contains(string(projected.Data), value) {
			t.Errorf("lost extraction control or bounded metadata: %s", value)
		}
	}
	if !strings.Contains(string(out.Data), secret) {
		t.Fatal("projection modified original durable record")
	}
}

func TestCrawlerJSONStructureDoesNotPreviewLongProse(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"url": "https://example.test/detail", "arbitrary_name": strings.Repeat("private article prose ", 50)})
	out, _ := toolData(ToolCall{ID: "json-source", Function: FunctionCall{Name: "inspect_source"}}, inspectResponse(acquisition.Response{Body: string(body), Receipt: acquisition.Receipt{URL: "https://example.test/api", Method: "GET", Status: 200}}, acquisition.Stats{}))
	r := agentdata.Run{BudgetJSON: `{"loop_version":5}`}
	out = modelObservationForRun(r, out, nil)
	if strings.Contains(string(out.Data), "private article prose") {
		t.Fatal("JSON structure exposed prose preview")
	}
	if !strings.Contains(string(out.Data), "https://example.test/detail") || !strings.Contains(string(out.Data), "arbitrary_name") {
		t.Fatal("JSON structure lost bounded locators or field shape")
	}
}

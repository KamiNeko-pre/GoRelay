package aiapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
)

func TestCrawlerLoopEmbeddingDoesNotRequireRAGVectors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float32{1, 0, 0}}}})
	}))
	defer server.Close()
	t.Setenv("GORELAY_EMBED_BASE_URL", server.URL)
	t.Setenv("GORELAY_EMBED_MODEL", "test-local")
	t.Setenv("GORELAY_EMBED_API_KEY", "test-only")
	t.Setenv("GORELAY_QDRANT_URL", "")
	s := NewService(nil)
	s.Configure()
	if s.Retriever != nil {
		t.Fatal("invalid vectors unexpectedly enabled RAG")
	}
	if s.embedder() == nil {
		t.Fatal("crawler loop embedding was disabled by missing RAG vectors")
	}
	vectors, err := s.embedder().EmbedTexts(context.Background(), []string{"a completed crawler decision"})
	if err != nil || len(vectors) != 1 {
		t.Fatal("independent embedding request failed", err)
	}
}

func TestCrawlerCorrectionDoesNotReviveResolvedFailures(t *testing.T) {
	s := efficiencyState()
	failed := ToolResult{CallID: "bad", Name: "validate_program", Error: "missing budget field", Failure: &FailureInfo{Code: "PROGRAM_OUTPUT_CONTRACT_ERROR", Details: map[string]any{"field": "budget"}, Recovery: &ToolRecovery{Strategy: "correct_program", Instruction: "Fix the budget field"}}}
	good := ToolResult{CallID: "good", Name: "validate_program", RepairOf: "bad", Data: json.RawMessage(`{"id":"candidate-real","artifact_hash":"hash-real","next_action":{"tool":"register_source","arguments":{"candidate_id":"candidate-real","description":"collect budgets"}}}`)}
	for i, out := range []ToolResult{failed, good} {
		raw, _ := json.Marshal(out)
		stage := "succeeded"
		if out.Error != "" {
			stage = "failed"
		}
		s.Operations = append(s.Operations, agentdata.Operation{ID: uint64(i + 1), RunID: s.Run.ID, Key: "tool:" + out.CallID, Kind: out.Name, Stage: stage, ResultJSON: string(raw)})
	}
	brief := buildCorrectionBrief(s, "correcting", "no accepted records yet")
	raw, _ := json.Marshal(brief)
	if strings.Contains(string(raw), "Fix the budget field") {
		t.Error("correction prioritized an already repaired failure")
	}
	if !strings.Contains(string(raw), "candidate-real") || !strings.Contains(string(raw), `"methodology"`) {
		t.Error("correction omitted the exact saved candidate and actionable methodology", string(raw))
	}
}

func TestCrawlerCorrectionReferencesStructureWithoutInvitingProseReads(t *testing.T) {
	s := efficiencyState()
	out, _ := toolData(ToolCall{ID: "page", Function: FunctionCall{Name: "inspect_source"}}, inspectResponse(acquisition.Response{Receipt: acquisition.Receipt{URL: "https://public.example/detail", Method: "GET", Status: 200}, Body: `<table><tr><th>Budget</th><td>26 million</td></tr></table>`}, acquisition.Stats{}))
	raw, _ := json.Marshal(out)
	s.Operations = []agentdata.Operation{{ID: 1, RunID: s.Run.ID, Key: "tool:page", Kind: "inspect_source", Stage: "succeeded", ResultJSON: string(raw)}}
	brief, _ := json.Marshal(buildCorrectionBrief(s, "correcting", "no records"))
	if strings.Contains(string(brief), "visible text, readable") || !strings.Contains(string(brief), "page") || !strings.Contains(string(brief), "methodology") {
		t.Fatal("brief invites prose reading or lacks grounded steps", string(brief))
	}
}

func TestCrawlerCanInspectShortTableLabelsWithoutReadingLongProse(t *testing.T) {
	page := rowTestPage(`<table><tr><th>项目编号</th><td>ABC-52</td></tr></table><article><p>`+strings.Repeat("long prose ", 100)+`</p></article>`, "https://example.test/detail")
	catalog := crawlerCatalog(page.Source.Body, completeSourceSelectorCatalog(page.Source.Body))
	labels := []string{}
	for _, entry := range catalog {
		labels = append(labels, entry.Sample)
	}
	all := strings.Join(labels, "|")
	if !strings.Contains(all, "ABC-52") || !strings.Contains(all, "项目编号") || strings.Contains(all, "long prose") {
		t.Fatal("table fields unavailable or prose leaked", all)
	}
}

func TestCrawlerPromptSurfaceDoesNotAdvertiseUnavailablePaths(t *testing.T) {
	s := efficiencyState()
	for _, tool := range runToolDefinitions(s.Run) {
		if tool.Function.Name == "read_source_experience" && strings.Contains(tool.Function.Description, "inspect_source(source_id)") {
			t.Error("experience still advertises an unreadable program inspection")
		}
		if tool.Function.Name == "finish_task" && strings.Contains(tool.Function.Description, "analysis goals") {
			t.Error("crawler finish still sends model into analysis workflow")
		}
	}
	prompt := runSystemPrompt(s.Run)
	if !strings.Contains(prompt, "source_text(html)") || strings.Contains(prompt, "no html_attr/html_text/DOM helper") && !strings.Contains(prompt, "source_text(html) ->") {
		t.Error("exact sandbox list omits a supported helper used by its own instructions")
	}
}

func TestCrawlerSemanticFailureIsObservableNotNovelty(t *testing.T) {
	s := efficiencyState()
	s.Operations = []agentdata.Operation{modelRound("one"), modelRound("two")}
	service := serviceWithEmbedder(&fixedEmbedder{fail: true})
	check := service.assessSemanticRepeat(context.Background(), s)
	if check.Status != "error" || check.Error == "" || check.Repeated {
		t.Fatalf("embedding failure masquerades as novelty: %+v", check)
	}
	service = &Service{}
	if check := service.assessSemanticRepeat(context.Background(), s); check.Status != "not_configured" {
		t.Fatal(check)
	}
}

package agentbench

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"testing"
)

func TestSummaryReportsBackgroundIndexUsageAndMissingAccounting(t *testing.T) {
	metrics, _ := json.Marshal(knowledge.IndexMetrics{EmbeddingCalls: 1, Usage: &knowledge.EmbeddingUsage{TotalTokens: 77}})
	good := true
	rows := []Trial{{CaseID: "a", Status: "completed", Correct: &good, Accounting: &aiapp.RunAccounting{IndexSettled: false, IndexAttempts: []knowledge.IndexAttempt{{MetricsJSON: string(metrics)}}}}, {CaseID: "b", Status: "failed"}}
	raw, _ := json.Marshal(Summarize(rows))
	var groups []map[string]any
	json.Unmarshal(raw, &groups)
	if groups[0]["reported_index_embedding_tokens"] != float64(77) || groups[0]["pending_index_runs"] != float64(1) || groups[0]["missing_accounting_runs"] != float64(1) {
		t.Fatalf("index costs still invisible in task summary: %s", raw)
	}
}

func TestSummaryIncludesSearchModelTokensAndProtectsNativeSearch(t *testing.T) {
	good := true
	for _, tc := range []struct {
		name       string
		search     aiapp.SearchResponse
		wantTokens int64
		unknown    bool
	}{
		{"model_search", aiapp.SearchResponse{Model: "search-model", Usage: &aiapp.Usage{PromptTokens: 80, CachedPromptTokens: 20, CompletionTokens: 20, TotalTokens: 100}}, 110, false},
		{"different_model_search", aiapp.SearchResponse{Model: "other-model", Usage: &aiapp.Usage{PromptTokens: 190, CompletionTokens: 10, TotalTokens: 200}}, 210, false},
		{"unknown_search_usage", aiapp.SearchResponse{Model: "search-model"}, 10, true},
		{"native_search", aiapp.SearchResponse{}, 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(tc.search)
			receipt, _ := json.Marshal(aiapp.ToolResult{Data: data})
			rows := []Trial{{CaseID: "goal", Status: "completed", Correct: &good, Operations: []agentdata.Operation{{Kind: "model", UsageJSON: `{"total_tokens":10}`}, {Kind: "discover_sources", ResultJSON: string(receipt)}}}}
			got := Summarize(rows)[0]
			if (got.TokensPerCorrectTask == nil) != tc.unknown || (!tc.unknown && *got.TokensPerCorrectTask != float64(tc.wantTokens)) {
				t.Fatalf("search tokens omitted or unknown usage called zero: %+v", got)
			}
		})
	}
}

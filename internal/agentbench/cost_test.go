package agentbench

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"math"
	"testing"
)

func TestEstimatedWholeTaskCostIncludesFailuresIndexAndSearchOrStaysUnknown(t *testing.T) {
	one := 1.0
	two := 2.0
	search := 0.25
	book := PriceBook{Currency: "test_units", Date: "fixture", Models: map[string]ModelPrice{"chat": {InputPerMillion: &one, OutputPerMillion: &two}, "embed": {InputPerMillion: &one}}, SearchPerCall: &search}
	usage, _ := json.Marshal(aiapp.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150})
	response, _ := json.Marshal(aiapp.ChatResponse{Model: "chat"})
	metrics, _ := json.Marshal(knowledge.IndexMetrics{EmbeddingCalls: 1, Model: "embed", Usage: &knowledge.EmbeddingUsage{PromptTokens: 40, TotalTokens: 40}})
	good := true
	bad := false
	rows := []Trial{{CaseID: "ok", Correct: &good, Status: "completed", Operations: []agentdata.Operation{{Kind: "model", UsageJSON: string(usage), ResultJSON: string(response)}, {Kind: "discover_sources"}}, Accounting: &aiapp.RunAccounting{IndexSettled: true, IndexAttempts: []knowledge.IndexAttempt{{MetricsJSON: string(metrics)}}}}, {CaseID: "failed", Correct: &bad, Status: "failed", Operations: []agentdata.Operation{{Kind: "model", UsageJSON: string(usage), ResultJSON: string(response)}}, Accounting: &aiapp.RunAccounting{IndexSettled: true}}}
	cost := EstimateCosts(rows, book)
	if cost.Total == nil || cost.PerCorrectTask == nil || *cost.Total < 0.25043 || *cost.Total > 0.25045 {
		t.Fatalf("failed or index/search cost omitted: %+v", cost)
	}
	rows[1].Operations[0].UsageJSON = "null"
	if result := EstimateCosts(rows, book); result.Total != nil || result.PerCorrectTask != nil || result.UnknownComponents != 1 {
		t.Fatal("unknown usage turned into zero")
	}
	rows[1].Operations = nil
	rows[0].Accounting.IndexSettled = false
	if EstimateCosts(rows, book).Total != nil {
		t.Fatal("pending future index cost treated as final")
	}
}

func TestSearchModelCostIsIncludedAlongsideSearchFee(t *testing.T) {
	input, output, fee := 2.0, 4.0, 0.1
	book := PriceBook{Currency: "fixture", Date: "fixture", Models: map[string]ModelPrice{"search-model": {InputPerMillion: &input, OutputPerMillion: &output}}, SearchPerCall: &fee}
	for _, tc := range []struct {
		name, stage string
		response    aiapp.SearchResponse
		want        float64
		unknown     bool
	}{
		{"model_search", "succeeded", aiapp.SearchResponse{Model: "search-model", Usage: &aiapp.Usage{PromptTokens: 1000, CompletionTokens: 100, TotalTokens: 1100}}, 0.1024, false},
		{"failed_model_search", "failed", aiapp.SearchResponse{Model: "search-model", Usage: &aiapp.Usage{PromptTokens: 2000, CompletionTokens: 50, TotalTokens: 2050}}, 0.1042, false},
		{"unknown_model_usage", "failed", aiapp.SearchResponse{Model: "search-model"}, 0.1, true},
		{"ordinary_search", "succeeded", aiapp.SearchResponse{}, 0.1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(tc.response)
			receipt, _ := json.Marshal(aiapp.ToolResult{Data: data})
			rows := []Trial{{Operations: []agentdata.Operation{{Kind: "discover_sources", Stage: tc.stage, ResultJSON: string(receipt)}}, Accounting: &aiapp.RunAccounting{IndexSettled: true}}}
			got := EstimateCosts(rows, book)
			if math.Abs(got.KnownSubtotal-tc.want) > 1e-9 || (got.Total == nil) != tc.unknown {
				t.Fatalf("search cost lost or ordinary search changed: %+v, want %v unknown %v", got, tc.want, tc.unknown)
			}
		})
	}
}

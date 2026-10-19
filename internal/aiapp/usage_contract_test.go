package aiapp

import (
	"encoding/json"
	"go-lua-crawler/internal/agentdata"
	"testing"
)

func TestMetricsIncludeSearchUsageWithoutTurningUnknownIntoZero(t *testing.T) {
	usage, _ := json.Marshal(Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12})
	search, _ := toolData(ToolCall{ID: "search", Function: FunctionCall{Name: "discover_sources"}}, SearchResponse{Usage: &Usage{PromptTokens: 5, CompletionTokens: 1, TotalTokens: 6}})
	raw, _ := json.Marshal(search)
	ops := []agentdata.Operation{{Kind: "model", Stage: "succeeded", UsageJSON: string(usage)}, {Kind: "discover_sources", Stage: "succeeded", ResultJSON: string(raw)}, {Kind: "discover_sources", Stage: "failed", ResultJSON: `{"name":"discover_sources","error":"unavailable"}`}}
	out := OperationMetrics(ops)
	if out["reported_total_tokens"] != int64(18) {
		t.Fatalf("search usage omitted: %+v", out)
	}
	if out["usage_unknown_attempts"] != 1 {
		t.Fatalf("unknown usage not accounted: %+v", out)
	}
}

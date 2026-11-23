package agentbench

import (
	"encoding/json"
	"math"
	"testing"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/information"
	"go-lua-crawler/internal/knowledge"
)

func TestWholeRequestAccountingCountsEachModelOnceAndKeepsUnknown(t *testing.T) {
	var usage aiapp.Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":30},"completion_tokens_details":{"reasoning_tokens":12}}`), &usage); err != nil {
		t.Fatal(err)
	}
	correct := true
	row := Trial{CaseID: "whole", Status: "completed", Correct: &correct, Operations: []agentdata.Operation{{Kind: "model", UsageJSON: string(mustTestJSON(usage)), ResultJSON: `{"model":"chat"}`}}, InformationAccounting: &information.RequestAccounting{
		ModelCalls: []aiapp.ModelCallAccounting{
			{ID: "upper-route", Scope: "information", Purpose: "choose_route", Model: "chat", Usage: &usage},
			{ID: "child-call", Scope: "crawler", Purpose: "model", Model: "chat", Usage: &usage},
			{ID: "upper-answer", Scope: "information", Purpose: "answer_information", Model: "chat", Usage: &usage},
			{ID: "failed-search", Scope: "information", Purpose: "search_api", Model: "chat", Stage: "failed", Usage: &usage},
		}, SearchCalls: []string{"failed-search"}, RunAccounting: &aiapp.RunAccounting{IndexSettled: true},
		Retrievals: []information.RetrievalAccounting{{ID: "upper-rag", Metrics: &knowledge.RetrievalMetrics{EmbeddingCalls: 1, Model: "embed", Usage: &knowledge.EmbeddingUsage{PromptTokens: 7, TotalTokens: 7}}}},
	}}
	s := Summarize([]Trial{row})[0]
	if s.ModelAttempts != 3 || s.SearchModelAttempts != 1 || s.LLMReportedTokens != 480 || s.LLMCachedPromptTokens != 120 || s.ReasoningTokens != 48 || s.SearchAttempts != 1 || s.EmbeddingTokens != 7 {
		t.Fatalf("lost or duplicated call: %+v", s)
	}
	if s.TokensPerCorrectTask == nil || *s.TokensPerCorrectTask != 480 {
		t.Fatal("reasoning was added twice")
	}
	input, output, fee := 1.0, 2.0, 0.25
	book := PriceBook{Currency: "fixture", Date: "fixture", Models: map[string]ModelPrice{"chat": {InputPerMillion: &input, OutputPerMillion: &output}, "embed": {InputPerMillion: &input}}, SearchPerCall: &fee}
	cost := EstimateCosts([]Trial{row}, book)
	if cost.Total == nil || math.Abs(*cost.Total-0.250567) > 1e-10 {
		t.Fatalf("wrong whole cost %+v", cost)
	}
	row.InformationAccounting.ModelCalls = append(row.InformationAccounting.ModelCalls, aiapp.ModelCallAccounting{ID: "unknown", Purpose: "choose_route", Stage: "reserved"})
	s = Summarize([]Trial{row})[0]
	if s.UnknownUsageAttempts != 1 || s.TokensPerCorrectTask != nil {
		t.Fatal("unknown usage became zero")
	}
	if got := EstimateCosts([]Trial{row}, book); got.Total != nil || got.UnknownComponents != 1 {
		t.Fatalf("unknown cost hidden %+v", got)
	}
}

func TestLegacyCrawlerSummaryUsesNestedCacheWithoutRequiringInformationLayer(t *testing.T) {
	usage := `{"prompt_tokens":100,"total_tokens":100,"prompt_tokens_details":{"cached_tokens":40}}`
	row := Trial{CaseID: "crawler", Operations: []agentdata.Operation{{Kind: "model", UsageJSON: usage, ResultJSON: `{"model":"crawler"}`}}, Accounting: &aiapp.RunAccounting{IndexSettled: true}}
	s := Summarize([]Trial{row})[0]
	if s.ModelAttempts != 1 || s.LLMReportedTokens != 100 || s.LLMCachedPromptTokens != 40 {
		t.Fatalf("crawler path changed %+v", s)
	}
}

func mustTestJSON(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}

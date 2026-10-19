package information

import (
	"context"
	"testing"

	"go-lua-crawler/internal/knowledge"
)

type measuredLookupEmbedder struct{}

func (measuredLookupEmbedder) EmbedTexts(context.Context, []string) ([][]float32, error) {
	return [][]float32{{1, 0}}, nil
}
func (measuredLookupEmbedder) EmbedMeasured(context.Context, []string) (knowledge.EmbeddingMeasurement, error) {
	return knowledge.EmbeddingMeasurement{Vectors: [][]float32{{1, 0}}, Model: "local-test-embed", Usage: &knowledge.EmbeddingUsage{PromptTokens: 17, TotalTokens: 17}}, nil
}

func TestFailedPreflightRetainsCompletedEmbeddingUsage(t *testing.T) {
	s, v := retrievalService(t)
	s.Retriever.Embed = measuredLookupEmbedder{}
	v.fail = true
	r, err := s.Create(context.Background(), Input{RequestKey: "failed-lookup-usage", Goal: "find the observed release", SourceScope: []string{"source.example"}})
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := s.Preflight(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if lookup.RetrievalStatus != "unavailable" || len(lookup.Evidence.Entries) != 0 {
		t.Fatal("failed vector search became valid evidence")
	}
	got, err := s.Accounting(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Retrievals) != 1 || got.Retrievals[0].Metrics == nil || got.Retrievals[0].Metrics.EmbeddingCalls != 1 || got.Retrievals[0].Metrics.Usage == nil || got.Retrievals[0].Metrics.Usage.TotalTokens != 17 {
		t.Fatalf("paid or measured embedding omitted after vector failure: %+v", got.Retrievals)
	}
}

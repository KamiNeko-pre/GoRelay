package knowledge

import (
	"context"
	"errors"
	"testing"
)

type searchFailureFixture struct{}

func (*searchFailureFixture) Upsert(context.Context, []VectorPoint) error { return nil }
func (*searchFailureFixture) Search(context.Context, []float32, int) ([]VectorHit, error) {
	return nil, errors.New("vector service unavailable")
}

func TestRetrievalRetainsEmbeddingChargeOnDownstreamFailure(t *testing.T) {
	for _, query := range []string{"query", "different sibling query"} {
		vectors := &searchFailureFixture{}
		r := Retriever{Embed: &meteredEmbeddingFixture{}, Vectors: vectors}
		out, err := r.Retrieve(context.Background(), query, 2)
		if err == nil || out.Metrics.EmbeddingCalls != 1 || out.Metrics.Usage == nil || out.Metrics.Usage.TotalTokens != 15 || len(out.Entries) != 0 {
			t.Fatalf("charge lost on downstream failure: %+v %v", out, err)
		}
	}
	r := Retriever{Embed: &meteredEmbeddingFixture{}}
	if out, err := r.Retrieve(context.Background(), "query", 0); err == nil || out.Metrics.EmbeddingCalls != 0 {
		t.Fatal("invalid request made a billed call")
	}
}

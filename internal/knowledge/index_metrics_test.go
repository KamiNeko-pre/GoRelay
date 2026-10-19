package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/testdb"
	"testing"
)

type meteredEmbeddingFixture struct{ calls int }

func (e *meteredEmbeddingFixture) EmbedTexts(ctx context.Context, texts []string) ([][]float32, error) {
	out, err := e.EmbedMeasured(ctx, texts)
	return out.Vectors, err
}
func (e *meteredEmbeddingFixture) EmbedMeasured(context.Context, []string) (EmbeddingMeasurement, error) {
	e.calls++
	return EmbeddingMeasurement{Vectors: [][]float32{{1, 0}}, Model: "fixture", RequestID: "fixture-request", Usage: &EmbeddingUsage{PromptTokens: 15, TotalTokens: 15}}, nil
}

func TestIndexMetricsRetainFailedAttemptUsageAndReuse(t *testing.T) {
	s := NewStore(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	doc, _ := NewDocument(Input{Source: "fixture", SourceKey: "metrics", Title: "first", Body: "body", Format: "text"})
	if _, err := s.Put(ctx, doc, "fp"); err != nil {
		t.Fatal(err)
	}
	embed := &meteredEmbeddingFixture{}
	vectors := &testVectors{fail: true, points: map[string]VectorPoint{}}
	// Only a test planner; BuildIndexPlan stays learner-owned.
	worker := Indexer{Store: s, Embed: embed, Vectors: vectors, Fingerprint: "fp", Planner: func(_ context.Context, in IndexInput) (IndexPlan, error) {
		if in.Parent != nil {
			return IndexPlan{Mode: "reuse", ParentGeneration: in.Parent.Generation}, nil
		}
		return IndexPlan{Mode: "rebuild"}, nil
	}}
	if err := worker.Once(ctx); err == nil {
		t.Fatal("vector failure hidden")
	}
	if err := s.Retry(ctx, doc.ID); err != nil {
		t.Fatal(err)
	}
	vectors.fail = false
	if err := worker.Once(ctx); err != nil {
		t.Fatal(err)
	}
	changed, _ := NewDocument(Input{Source: "fixture", SourceKey: "metrics", Title: "new metadata", Body: "body", Format: "text"})
	if _, err := s.Put(ctx, changed, "fp"); err != nil {
		t.Fatal(err)
	}
	if err := worker.Once(ctx); err != nil {
		t.Fatal(err)
	}
	attempts, err := s.IndexAttempts(ctx, doc.ID)
	if err != nil || len(attempts) != 3 {
		t.Fatalf("attempt ledger lost: %v %+v", err, attempts)
	}
	var first, last IndexMetrics
	if err := json.Unmarshal([]byte(attempts[0].MetricsJSON), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(attempts[2].MetricsJSON), &last); err != nil {
		t.Fatal(err)
	}
	if attempts[0].Status != "failed" || first.Usage == nil || first.Usage.TotalTokens != 15 || first.EmbeddingCalls != 1 {
		t.Fatalf("failed embedding cost discarded: %+v", first)
	}
	if attempts[2].Status != "succeeded" || last.PlanMode != "reuse" || last.EmbeddingCalls != 0 || last.Usage != nil || embed.calls != 2 {
		t.Fatalf("reuse fabricated a billed request: %+v", last)
	}
}

func TestIndexMetricsUnknownUsageAndPendingCore(t *testing.T) {
	s := NewStore(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	doc, _ := NewDocument(Input{Source: "fixture", SourceKey: "unknown", Title: "A", Body: "body", Format: "text"})
	s.Put(ctx, doc, "fp")
	w := Indexer{Store: s, Embed: &testEmbed{}, Vectors: &testVectors{points: map[string]VectorPoint{}}, Fingerprint: "fp", Planner: func(context.Context, IndexInput) (IndexPlan, error) { return IndexPlan{}, ErrCorePending }}
	if err := w.Once(ctx); !errors.Is(err, ErrCorePending) {
		t.Fatalf("learner core bypassed: %v", err)
	}
	s.Retry(ctx, doc.ID)
	w.Planner = nil // Exercise the production planner after the injected outage.
	if err := w.Once(ctx); err != nil {
		t.Fatal(err)
	}
	attempts, _ := s.IndexAttempts(ctx, doc.ID)
	var pending, actual IndexMetrics
	json.Unmarshal([]byte(attempts[0].MetricsJSON), &pending)
	json.Unmarshal([]byte(attempts[1].MetricsJSON), &actual)
	if pending.EmbeddingCalls != 0 || actual.EmbeddingCalls != 1 || actual.Usage != nil || actual.EmbeddingOutcome != "returned" {
		t.Fatal("unknown usage became zero or pending triggered embedding")
	}
}

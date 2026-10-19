package knowledge

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
)

type scopeBatchEmbed struct{}

func (scopeBatchEmbed) EmbedTexts(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

// Controlled vectors make stale-candidate starvation deterministic. SQL,
// publication and Qdrant are real; no model action or answer is mocked.
func TestCurrentGenerationRetrievalCannotBeStarvedByResidualVectors(t *testing.T) {
	endpoint := os.Getenv("QDRANT_INTEGRATION_URL")
	if endpoint == "" {
		t.Skip("QDRANT_INTEGRATION_URL required")
	}
	ctx := context.Background()
	collection := "gorelay_scope_" + agentdata.Token()
	q, err := NewQdrant(endpoint, collection, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := q.EnsureCollection(ctx, 2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		req, _ := http.NewRequest("DELETE", endpoint+"/collections/"+collection, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
	})
	store := NewStore(testdb.Open(t))
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	indexer := Indexer{Store: store, Embed: scopeBatchEmbed{}, Vectors: q, Fingerprint: "scope-test", ChunkSize: 40, ChunkOverlap: 0}
	var docs []Document
	for _, in := range []Input{
		{Source: "old", SourceKey: "withdrawn", Title: "withdrawn", Body: strings.Repeat("withdrawn rate ", 240), Format: "text"},
		{Source: "alpha", SourceKey: "rate", URL: "https://alpha.example/rate", Title: "alpha rate", Body: "alpha-current rate", Format: "text"},
		{Source: "beta", SourceKey: "rate", URL: "https://beta.example/rate", Title: "beta rate", Body: "beta-current rate", Format: "text"},
	} {
		doc, err := NewDocument(in)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(ctx, doc, indexer.Fingerprint); err != nil {
			t.Fatal(err)
		}
		if err := indexer.Once(ctx); err != nil {
			t.Fatal(err)
		}
		docs = append(docs, doc)
		if in.Source != "old" {
			h, _ := store.Head(ctx, doc.ID)
			chunks, _ := store.GenerationChunks(ctx, h.Generation)
			for _, c := range chunks {
				if err := q.Upsert(ctx, []VectorPoint{{ID: c.ID, Vector: []float32{0.5, 1}, Payload: map[string]any{"generation": c.Generation}}}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := store.Invalidate(ctx, docs[0].ID); err != nil {
		t.Fatal(err)
	}
	r := Retriever{Store: store, Embed: &testEmbed{}, Vectors: q, Lexical: &LexicalCache{}}
	for _, query := range []string{"rate", "different query"} {
		set, err := r.Retrieve(ctx, query, 2)
		if err != nil || len(set.Entries) != 2 {
			t.Fatalf("current evidence starved by withdrawn vectors: entries=%d err=%v", len(set.Entries), err)
		}
	}
	for _, mode := range []string{"dense", "bm25", "hybrid"} {
		for _, source := range []string{"alpha", "beta"} {
			policy := RetrievalPolicy{Mode: mode, CandidateBudget: 60, Scope: RetrievalScope{URLHosts: []string{source + ".example"}}}
			set, err := r.RetrieveWithPolicy(ctx, "rate", 2, policy)
			if err != nil || len(set.Entries) != 1 || set.Entries[0].SourceID != source || set.Metrics.UniqueCandidates > 60 {
				t.Fatalf("scope/candidate contract violated for %s %s: %+v %v", mode, source, set, err)
			}
			if mode == "bm25" && set.Metrics.EmbeddingCalls != 0 {
				t.Fatal("BM25 made an embedding call")
			}
		}
	}
}

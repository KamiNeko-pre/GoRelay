package knowledge

import (
	"context"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"net/http"
	"os"
	"testing"
)

func TestRealQdrantPublicationAndSQLCurrentShield(t *testing.T) {
	endpoint := os.Getenv("QDRANT_INTEGRATION_URL")
	if endpoint == "" {
		t.Skip("QDRANT_INTEGRATION_URL required")
	}
	collection := "gorelay_test_" + agentdata.Token()
	q, err := NewQdrant(endpoint, collection, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err = q.EnsureCollection(ctx, 2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		req, _ := http.NewRequest("DELETE", endpoint+"/collections/"+collection, nil)
		resp, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Error(e)
		} else {
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Error(resp.Status)
			}
		}
	})
	if err = q.EnsureCollection(ctx, 3); err == nil {
		t.Fatal("model dimension mismatch accepted")
	}
	s := NewStore(testdb.Open(t))
	if err = s.Migrate(); err != nil {
		t.Fatal(err)
	}
	doc, _ := NewDocument(Input{Source: "fixture", SourceKey: "live-protocol", URL: "https://example.com/a", Title: "A", Body: "fixture body", Format: "text"})
	if _, err = s.Put(ctx, doc, "fixture"); err != nil {
		t.Fatal(err)
	}
	indexer := Indexer{Store: s, Embed: &testEmbed{}, Vectors: q, Fingerprint: "fixture", Planner: func(context.Context, IndexInput) (IndexPlan, error) { return IndexPlan{Mode: "rebuild"}, nil }}
	if err = indexer.Once(ctx); err != nil {
		t.Fatal(err)
	}
	r := Retriever{Store: s, Embed: &testEmbed{}, Vectors: q}
	set, err := r.Retrieve(ctx, "body", 2)
	if err != nil || len(set.Entries) != 1 {
		t.Fatalf("published chunk unavailable: %+v %v", set, err)
	}
	if err = s.Invalidate(ctx, doc.ID); err != nil {
		t.Fatal(err)
	}
	hits, err := q.Search(ctx, []float32{1, 0}, 2)
	if err != nil || len(hits) != 1 {
		t.Fatal("fixture requires residual vector")
	}
	set, err = r.Retrieve(ctx, "body", 2)
	if err != nil || len(set.Entries) != 0 {
		t.Fatal("residual vector escaped SQL current-state shield")
	}
}

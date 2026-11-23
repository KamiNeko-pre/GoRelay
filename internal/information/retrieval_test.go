package information

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/testdb"
)

// These are transport substitutes for the retrieval component, not an oracle
// for model actions. The real indexer, SQL publication and retriever still run.
type testVectors struct {
	points  []knowledge.VectorPoint
	queries int
	fail    bool
}

func (v *testVectors) Upsert(_ context.Context, p []knowledge.VectorPoint) error {
	v.points = append(v.points, p...)
	return nil
}
func (v *testVectors) Search(_ context.Context, _ []float32, n int) ([]knowledge.VectorHit, error) {
	v.queries++
	if v.fail {
		return nil, fmt.Errorf("vector service unavailable")
	}
	out := []knowledge.VectorHit{}
	for _, p := range v.points {
		out = append(out, knowledge.VectorHit{ID: p.ID, Score: 1})
		if len(out) == n {
			break
		}
	}
	return out, nil
}

type testEmbed struct{}

func (testEmbed) EmbedTexts(_ context.Context, texts []string) ([][]float32, error) {
	rows := make([][]float32, len(texts))
	for i := range rows {
		rows[i] = []float32{1, 0}
	}
	return rows, nil
}

func retrievalService(t *testing.T) (*Service, *testVectors) {
	t.Helper()
	crawler := aiapp.NewService(testdb.Open(t))
	if err := crawler.Migrate(); err != nil {
		t.Fatal(err)
	}
	s := NewService(crawler)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	v := &testVectors{}
	s.Retriever = &knowledge.Retriever{Store: crawler.Knowledge, Embed: testEmbed{}, Vectors: v}
	return s, v
}

func TestInformationInputIdempotencyAndCrawlerSeparation(t *testing.T) {
	s, _ := retrievalService(t)
	ctx := context.Background()
	in := Input{RequestKey: "input-key", Goal: "get public data", SourceScope: []string{"source.example"}}
	a, err := s.Create(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, in)
	if err != nil || a.ID != b.ID {
		t.Fatal("same request did not restore", err)
	}
	in.Refresh = true
	if _, err = s.Create(ctx, in); err == nil {
		t.Fatal("request key changed original refresh contract")
	}
	var count int64
	s.DB.Model(&agentdata.Run{}).Count(&count)
	if count != 0 {
		t.Fatal("outer request changed crawler state")
	}
}

func mustJSON(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}

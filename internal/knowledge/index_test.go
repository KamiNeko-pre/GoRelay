package knowledge

import (
	"context"
	"errors"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestIndexGenerationPublicationLeaseAndInvalidation(t *testing.T) {
	db := testdb.Open(t)
	s := NewStore(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	doc, _ := NewDocument(Input{Source: "test", SourceKey: "a", Title: "A", Body: "first body", Format: "text", URL: "https://example.com/a"})
	head, err := s.Put(ctx, doc, "embed-v1")
	if err != nil {
		t.Fatal(err)
	}
	same, err := s.Put(ctx, doc, "embed-v1")
	if err != nil || same.Epoch != head.Epoch {
		t.Fatal("repeated import rebuilt")
	}
	work, err := s.Claim(ctx, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Publish(ctx, work, 1); err == nil {
		t.Fatal("published incomplete generation")
	}
	chunks := SplitDocument(doc, 600, 80, work.Generation)
	if err = s.SaveChunk(ctx, work, chunks[0], []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err = s.Publish(ctx, work, 1); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Head(ctx, doc.ID)
	if current.Generation != work.Generation {
		t.Fatal("generation not published")
	}
	doc2, _ := NewDocument(Input{Source: "test", SourceKey: "a", Title: "A changed metadata", Body: "first body", Format: "text", URL: "https://example.com/new"})
	newer, err := s.Put(ctx, doc2, "embed-v1")
	if err != nil || newer.Epoch <= head.Epoch {
		t.Fatal("metadata target did not advance")
	}
	if err = s.Publish(ctx, work, 1); !errors.Is(err, ErrIndexConflict) {
		t.Fatalf("old generation published: %v", err)
	}
	pending, _ := s.Claim(ctx, time.Minute)
	if err = s.Invalidate(ctx, doc.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.Publish(ctx, pending, 1); !errors.Is(err, ErrIndexConflict) {
		t.Fatalf("inactive document resurrected: %v", err)
	}
	visible, _ := s.CurrentChunks(ctx, []string{chunks[0].ID})
	if len(visible) != 0 {
		t.Fatal("residual vector became current evidence")
	}
}

func TestIndexerRecoveryAndMetadataVectorReuse(t *testing.T) {
	db := testdb.Open(t)
	s := NewStore(db)
	s.Migrate()
	ctx := context.Background()
	doc, _ := NewDocument(Input{Source: "test", SourceKey: "reuse", Title: "A", Body: "body content", Format: "text"})
	s.Put(ctx, doc, "fp1")
	vectors := &testVectors{points: map[string]VectorPoint{}}
	embed := &testEmbed{}
	// Fixture planner exercises the assistant-owned infrastructure only.
	planner := func(ctx context.Context, in IndexInput) (IndexPlan, error) {
		if in.Parent != nil && in.Parent.BodyHash == in.Head.BodyHash && in.Parent.Fingerprint == in.Head.Fingerprint {
			return IndexPlan{Mode: "reuse", ParentGeneration: in.Parent.Generation}, nil
		}
		return IndexPlan{Mode: "rebuild"}, nil
	}
	worker := Indexer{Store: s, Embed: embed, Vectors: vectors, Planner: planner, Fingerprint: "fp1"}
	vectors.fail = true
	if err := worker.Once(ctx); err == nil {
		t.Fatal("vector failure hidden")
	}
	head, _ := s.Head(ctx, doc.ID)
	if head.Generation != "" {
		t.Fatal("partial vector writes published")
	}
	s.Retry(ctx, doc.ID)
	vectors.fail = false
	if err := worker.Once(ctx); err != nil {
		t.Fatal(err)
	}
	previousCalls := embed.calls
	changed, _ := NewDocument(Input{Source: "test", SourceKey: "reuse", Title: "new title", Body: "body content", Format: "text"})
	s.Put(ctx, changed, "fp1")
	if err := worker.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if embed.calls != previousCalls {
		t.Fatal("metadata-only build re-embedded")
	}
	current, _ := s.Head(ctx, doc.ID)
	if current.Revision != changed.Revision || current.Generation == head.Generation {
		t.Fatal("did not publish fresh metadata generation")
	}
}

type testEmbed struct{ calls int }

func (e *testEmbed) EmbedTexts(context.Context, []string) ([][]float32, error) {
	e.calls++
	return [][]float32{{1, 0}}, nil
}

type testVectors struct {
	points map[string]VectorPoint
	fail   bool
}

func (v *testVectors) Upsert(ctx context.Context, points []VectorPoint) error {
	if v.fail {
		return errors.New("injected vector failure")
	}
	for _, p := range points {
		v.points[p.ID] = p
	}
	return nil
}
func (v *testVectors) Search(context.Context, []float32, int) ([]VectorHit, error) { return nil, nil }

func TestChunkOffsetsAndDeterministicIDs(t *testing.T) {
	doc := Document{ID: "id", Revision: "rev", Body: "中文段落\n第二段内容", Title: "title"}
	a := SplitDocument(doc, 5, 1, "g1")
	b := SplitDocument(doc, 5, 1, "g1")
	for i, c := range a {
		if c.ID != b[i].ID || c.Text != string([]rune(doc.Body)[c.Start:c.End]) {
			t.Fatal("unstable ID or broken unicode offsets")
		}
	}
	if a[0].ID == SplitDocument(doc, 5, 1, "g2")[0].ID {
		t.Fatal("generation points collide")
	}
}

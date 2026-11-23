package knowledge

import (
	"context"
	"errors"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestEvidenceRequestScopeAndCurrentVersion(t *testing.T) {
	db := testdb.Open(t)
	s := NewStore(db)
	s.Migrate()
	ctx := context.Background()
	doc, _ := NewDocument(Input{Source: "test", SourceKey: "evidence", Title: "A", Body: "supported text", Format: "text"})
	s.Put(ctx, doc, "fp")
	work, _ := s.Claim(ctx, time.Minute)
	chunk := SplitDocument(doc, 600, 80, work.Generation)[0]
	s.SaveChunk(ctx, work, chunk, []float32{1})
	s.Publish(ctx, work, 1)
	set := NewEvidenceSet([]Chunk{chunk}, map[string]Head{doc.ID: {ID: doc.ID, Revision: doc.Revision, Generation: work.Generation, Active: true, Title: "A"}})
	reader := SetReader{Store: s, Set: set}
	id := set.Entries[0].ID
	if _, err := reader.ReadEvidence(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadEvidence(ctx, "other-request-id"); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("foreign ID accepted: %v", err)
	}
	frozenText := set.Entries[0].Text
	s.Invalidate(ctx, doc.ID)
	if _, err := reader.ReadEvidence(ctx, id); !errors.Is(err, ErrInvalidReference) {
		t.Fatalf("inactive evidence returned: %v", err)
	}
	if set.Entries[0].Text != frozenText {
		t.Fatal("historical snapshot silently changed")
	}
}

func TestQdrantWritesWaitAndParsesQuery(t *testing.T) { testQdrantProtocol(t) }

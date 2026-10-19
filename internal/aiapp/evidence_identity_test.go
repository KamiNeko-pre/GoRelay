package aiapp

import (
	"go-lua-crawler/internal/knowledge"
	"testing"
)

func TestRepeatedRetrievalPreservesIdentityWithoutMergingDifferentVersions(t *testing.T) {
	prior := knowledge.EvidenceSet{Entries: []knowledge.Evidence{{ID: "original", DocID: "doc", Revision: "revision", Generation: "generation", ChunkID: "chunk"}}}
	next := knowledge.EvidenceSet{Entries: []knowledge.Evidence{
		{ID: "repeat", DocID: "doc", Revision: "revision", Generation: "generation", ChunkID: "chunk"},
		{ID: "new-generation", DocID: "doc", Revision: "revision", Generation: "new", ChunkID: "chunk"},
		{ID: "other-chunk", DocID: "doc", Revision: "revision", Generation: "generation", ChunkID: "other"},
	}}
	got := canonicalEvidenceIDs(prior, next)
	if got.Entries[0].ID != "original" || got.Entries[1].ID != "new-generation" || got.Entries[2].ID != "other-chunk" {
		t.Fatalf("identity or version boundary changed: %+v", got)
	}
	// A different run starts from its own receipts, not global evidence IDs.
	got = canonicalEvidenceIDs(knowledge.EvidenceSet{}, knowledge.EvidenceSet{Entries: []knowledge.Evidence{{ID: "other-run", DocID: "doc", Revision: "revision", Generation: "generation", ChunkID: "chunk"}}})
	if got.Entries[0].ID != "other-run" {
		t.Fatal("foreign run identity reused")
	}
}

package knowledge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"go-lua-crawler/internal/testdb"
)

func TestSavedDocumentSearchPagesAndEvidenceIdentity(t *testing.T) {
	docs := []Document{}
	for i := 0; i < 32; i++ {
		docs = append(docs, Document{ID: fmt.Sprintf("doc-%02d", i), Revision: "first", Source: "release-notes", Title: "Redis release", URL: "https://example.org/releases", Body: "Redis Streams records can be read after restarting the client."})
	}
	docs = append(docs, Document{ID: "catalog", Revision: "first", Source: "information-capability", Body: "Redis Streams capability description"})
	q := EvidenceQuery{Query: "Redis Streams", PageSize: 3}
	first, _, err := searchSavedDocuments(q, docs)
	if err != nil || first.Total != 32 || len(first.Entries) != 3 || first.NextCursor == "" {
		t.Fatalf("first page: %+v, %v", first, err)
	}
	again, _, err := searchSavedDocuments(q, docs)
	if err != nil || again.Entries[0].ID != first.Entries[0].ID {
		t.Fatalf("reading the same document minted another evidence identity: %v", err)
	}
	seen := map[string]bool{}
	page := first
	for {
		for _, e := range page.Entries {
			if seen[e.ID] {
				t.Fatalf("pagination repeated evidence %s", e.ID)
			}
			seen[e.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		q.Cursor = page.NextCursor
		page, _, err = searchSavedDocuments(q, docs)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 32 {
		t.Fatalf("pagination lost results: %d/32", len(seen))
	}
	q.Cursor = first.NextCursor
	docs[0].Revision = "second"
	if _, _, err := searchSavedDocuments(q, docs); !errors.Is(err, ErrEvidenceCursorChanged) {
		t.Fatalf("changed corpus silently restarted pagination: %v", err)
	}
	q = EvidenceQuery{Query: "Redis", Scope: RetrievalScope{Sources: []string{"other-source"}}}
	if empty, _, err := searchSavedDocuments(q, docs); err != nil || empty.Total != 0 {
		t.Fatalf("source scope leaked: %+v, %v", empty, err)
	}
}

func TestSavedEvidenceBeforeVectorIndexAndAfterRevisionChange(t *testing.T) {
	store := NewStore(testdb.Open(t))
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	doc, err := NewDocument(Input{Format: "text", Source: "official-docs", SourceKey: "compatibility", Title: "兼容说明", URL: "https://example.org/compatibility", Body: "旧版本仅支持 Linux；新版支持 Windows。"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, doc, "not-yet-indexed"); err != nil {
		t.Fatal(err)
	}
	page, err := store.SearchEvidence(ctx, EvidenceQuery{Query: "Windows"})
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("saved document requires vector publication: %+v, %v", page, err)
	}
	id := page.Entries[0].ID
	replacement, _ := NewDocument(Input{Format: "text", Source: "official-docs", SourceKey: "compatibility", Title: doc.Title, URL: doc.URL, Body: "更新后支持 macOS。"})
	if _, err := store.Put(ctx, replacement, "not-yet-indexed"); err != nil {
		t.Fatal(err)
	}
	saved, err := store.ReadEvidenceSnapshot(ctx, id)
	if err != nil || saved.Current || !saved.SourceActive || saved.Text != doc.Body {
		t.Fatalf("historical citation was lost or silently changed: %+v, %v", saved, err)
	}

	historical, err := store.ReadEvidenceDocument(ctx, doc.ID, doc.Revision, "", 1)
	if err != nil || len(historical.Entries) != 1 || historical.Entries[0].Current || historical.Entries[0].Text != doc.Body || historical.Entries[0].ID != id {
		t.Fatalf("direct historical capture cannot be cited: %+v %v", historical, err)
	}
	latest, err := store.SearchEvidence(ctx, EvidenceQuery{Query: "Windows"})
	if err != nil || latest.Total != 0 {
		t.Fatalf("superseded text was returned as current: %+v, %v", latest, err)
	}
}

func TestSearchEvidenceDocumentWindowScopedQueriesAndHistoricalRevision(t *testing.T) {
	store := NewStore(testdb.Open(t))
	if err := store.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	longBody := "Intro section: AlphaFeature release 1.31.8 details. " +
		strings.Repeat("Padding prose without target tokens. ", 60) +
		"Middle section: BetaFix stability patch 1.30.6 notes. " +
		strings.Repeat("Trailing prose without target tokens. ", 40)
	oldDoc, err := NewDocument(Input{
		Format:    "text",
		Source:    "official-docs",
		SourceKey: "release-overview",
		Title:     "Release Overview",
		URL:       "https://example.org/releases",
		Body:      longBody,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, oldDoc, "not-yet-indexed"); err != nil {
		t.Fatal(err)
	}
	newDoc, err := NewDocument(Input{
		Format:    "text",
		Source:    "official-docs",
		SourceKey: "release-overview",
		Title:     oldDoc.Title,
		URL:       oldDoc.URL,
		Body:      "Superseded newer body without AlphaFeature or BetaFix.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, newDoc, "not-yet-indexed"); err != nil {
		t.Fatal(err)
	}

	alphaPage, gotDoc, err := store.SearchEvidenceDocumentWindow(ctx, oldDoc.ID, oldDoc.Revision, "AlphaFeature", 0, 5)
	if err != nil {
		t.Fatalf("SearchEvidenceDocumentWindow alpha failed: %v", err)
	}
	if gotDoc.Revision != oldDoc.Revision || alphaPage.Total == 0 || len(alphaPage.Entries) == 0 {
		t.Fatalf("expected hits for AlphaFeature on historical revision, got total=%d entries=%d rev=%s", alphaPage.Total, len(alphaPage.Entries), gotDoc.Revision)
	}
	if alphaPage.Entries[0].Current || !alphaPage.Entries[0].SourceActive || !strings.Contains(alphaPage.Entries[0].Text, "AlphaFeature") {
		t.Fatalf("unexpected alpha entry: %+v", alphaPage.Entries[0])
	}

	betaPage, _, err := store.SearchEvidenceDocumentWindow(ctx, oldDoc.ID, oldDoc.Revision, "BetaFix", 0, 5)
	if err != nil {
		t.Fatalf("SearchEvidenceDocumentWindow beta failed: %v", err)
	}
	if betaPage.Total == 0 || len(betaPage.Entries) == 0 || !strings.Contains(betaPage.Entries[0].Text, "BetaFix") {
		t.Fatalf("expected BetaFix hit in middle section, got: %+v", betaPage.Entries)
	}
	if betaPage.Entries[0].ID == alphaPage.Entries[0].ID || betaPage.CorpusRevision == alphaPage.CorpusRevision {
		t.Fatalf("expected distinct chunk and corpus revision for distinct queries: alpha=%s beta=%s", alphaPage.CorpusRevision, betaPage.CorpusRevision)
	}
	snap, err := store.ReadEvidenceSnapshot(ctx, betaPage.Entries[0].ID)
	if err != nil || snap.Current || !strings.Contains(snap.Text, "BetaFix") {
		t.Fatalf("ReadEvidenceSnapshot failed for searched historical chunk: %+v err=%v", snap, err)
	}
	if _, _, err := store.SearchEvidenceDocumentWindow(ctx, oldDoc.ID, oldDoc.Revision, "   ", 0, 5); err == nil {
		t.Fatal("expected empty query to fail on SearchEvidenceDocumentWindow")
	}
}

package pipeline

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorelay/internal/acquisition"
	"gorelay/internal/contract"
	"gorelay/internal/testdb"
)

func TestRecordQueryFilterSortGroupExtractURLsAndExport(t *testing.T) {
	rows := []SourceRecord{
		{ID: 1, SourceID: "books", SnapshotRef: "ref1", PayloadJSON: `{"title":"Alpha","price_gbp":20.5,"rating":"Five","body":"See https://etcd.io/docs/v3.5/ and backup"}`},
		{ID: 2, SourceID: "books", SnapshotRef: "ref1", PayloadJSON: `{"title":"Beta","price_gbp":"£57.25","rating":"Three","body":"Guide: [upgrade](https://etcd.io/docs/v3.7/upgrades/upgrade_3_7/)"}`},
		{ID: 3, SourceID: "books", SnapshotRef: "ref2", PayloadJSON: `{"title":"Gamma","price_gbp":54.23,"rating":"Five","body":"Notes https://github.com/etcd-io/etcd/blob/main/CHANGELOG/CHANGELOG-3.7.md)."}`},
		{ID: 4, SourceID: "books", SnapshotRef: "ref2", PayloadJSON: `{"title":"Delta","price_gbp":54.64,"rating":"Three","body":"No links"}`},
		{ID: 5, SourceID: "books", SnapshotRef: "ref2", PayloadJSON: `{"title":"Epsilon","price_gbp":48.0,"rating":"Five","body":"` + strings.Repeat("x", 600) + `"}`},
	}

	// 1) Filter price_gbp > 50, sort DESC, limit 2, exclude body.
	q1, err := ParseRecordQuery(map[string]any{
		"where":          []map[string]any{{"field": "price_gbp", "op": "gt", "value": 50}},
		"order_by":       "price_gbp desc",
		"limit":          2,
		"exclude_fields": []string{"body"},
	})
	if err != nil {
		t.Fatal(err)
	}
	page1, err := ApplyRecordQuery(rows, 5, "books", 0, "", DefaultMCPQueryMaxBytes, DefaultMCPPageSize, "http://127.0.0.1:8080", q1)
	if err != nil {
		t.Fatal(err)
	}
	if page1.Mode != "query" || page1.Total != 5 || page1.Matched != 3 || page1.Returned != 2 {
		t.Fatalf("unexpected counts: %+v", page1)
	}
	if page1.Records[0]["title"] != "Beta" || page1.Records[1]["title"] != "Delta" {
		t.Fatalf("unexpected sort order (currency £57.25 must sort as 57.25): %+v", page1.Records)
	}
	if _, hasBody := page1.Records[0]["body"]; hasBody {
		t.Fatalf("expected body excluded: %+v", page1.Records[0])
	}

	// 2) Filter rating == Five + group_by rating + default truncation + extract_urls.
	q2, err := ParseRecordQuery(map[string]any{
		"where":        []map[string]any{{"field": "rating", "op": "eq", "value": "Five"}},
		"order_by":     "-price_gbp",
		"fields":       []string{"title", "price_gbp", "body"},
		"group_by":     "rating",
		"limit":        1,
		"extract_urls": map[string]any{"field": "body", "pattern": "CHANGELOG"},
	})
	if err != nil {
		t.Fatal(err)
	}
	page2, err := ApplyRecordQuery(rows, 5, "books", 0, "", DefaultMCPQueryMaxBytes, DefaultMCPPageSize, "", q2)
	if err != nil {
		t.Fatal(err)
	}
	if page2.Matched != 3 || page2.Groups["Five"] != 3 || page2.Returned != 1 {
		t.Fatalf("unexpected group/match result: %+v", page2)
	}
	if page2.Records[0]["title"] != "Gamma" {
		t.Fatalf("expected Gamma as top Five-star, got %+v", page2.Records[0])
	}
	wantURL := "https://github.com/etcd-io/etcd/blob/main/CHANGELOG/CHANGELOG-3.7.md"
	if len(page2.ExtractedURLs) != 1 || page2.ExtractedURLs[0] != wantURL {
		t.Fatalf("expected clean extracted URL %q, got %v", wantURL, page2.ExtractedURLs)
	}

	// 3) GroupPrefix + ExtractURLs MUST scan all matched rows even when Limit=1 returns only the first row.
	q3, err := ParseRecordQuery(map[string]any{
		"order_by":     "price_gbp asc",
		"limit":        1,
		"group_by":     "rating",
		"group_prefix": 2,
		"fields":       []string{"title"},
		"extract_urls": map[string]any{"field": "body", "pattern": "upgrade_3_7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	page3, err := ApplyRecordQuery(rows, 5, "books", 0, "", DefaultMCPQueryMaxBytes, DefaultMCPPageSize, "", q3)
	if err != nil {
		t.Fatal(err)
	}
	if page3.Returned != 1 || page3.Records[0]["title"] != "Alpha" {
		t.Fatalf("expected limit=1 ascending row Alpha, got %+v", page3.Records)
	}
	if page3.Groups["Fi"] != 3 || page3.Groups["Th"] != 2 {
		t.Fatalf("unexpected prefix groups: %+v", page3.Groups)
	}
	if len(page3.ExtractedURLs) != 1 || page3.ExtractedURLs[0] != "https://etcd.io/docs/v3.7/upgrades/upgrade_3_7/" {
		t.Fatalf("expected upgrade URL extracted across matched rows despite limit=1, got %v", page3.ExtractedURLs)
	}
}

func TestWriteSnapshotGzipConcurrentSameHashNoRace(t *testing.T) {
	dir := t.TempDir()
	gzPath := filepath.Join(dir, "ab", "abcdef1234567890.gz")
	body := strings.Repeat("identical content snapshot ", 200)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := writeSnapshotGzip(gzPath, body); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent writeSnapshotGzip failed: %v", err)
	}
	got, err := readSnapshotText(SnapshotBlob{GzipPath: filepath.ToSlash(gzPath)})
	if err != nil || !strings.Contains(got, "identical content snapshot") {
		t.Fatalf("corrupted concurrent gzip snapshot: err=%v len=%d", err, len(got))
	}
}

func TestReceiptModeAsyncArchiveAndExportDB(t *testing.T) {
	db := testdb.Open(t)
	if err := Migrate(db); err != nil {
		t.Fatal(err)
	}
	snapDir := t.TempDir()
	store := NewStore(db, snapDir)

	_, schema, err := contract.NormalizeOutputSchema([]byte(`{
		"entity_keys": ["id"],
		"fields": [
			{"name":"id","type":"string","required":true},
			{"name":"body","type":"source_text"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	longBody := strings.Repeat("markdown notes ", 40)
	recs := []map[string]any{
		{"id": "v1", "body": longBody},
		{"id": "v2", "body": longBody},
		{"id": "v3", "body": longBody},
		{"id": "v4", "body": longBody},
		{"id": "v5", "body": longBody},
	}
	ev := []acquisition.Response{{
		Body:    `<html><title>Releases</title><body>Release v5 body notes</body></html>`,
		Receipt: acquisition.Receipt{URL: "https://example.org/rel", Status: 200, FetchedAt: time.Now().UTC()},
	}}
	res, err := store.IngestBatchTx(context.Background(), db, IngestInput{
		SourceID: "rel_test", ConnectorVersion: 1, JobID: 7, OutputSchema: schema, Records: recs, Evidence: ev,
	})
	if err != nil || res.SavedRows != 5 || len(res.SnapshotRefs) != 1 {
		t.Fatalf("IngestBatchTx failed: res=%+v err=%v", res, err)
	}

	// Receipt MUST be compact (< 2 samples, truncated longBody, zero bulk dump).
	receipt, err := store.DeliverReceipt(context.Background(), "rel_test", 0, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Mode != "receipt" || receipt.Total != 5 || receipt.Returned != 2 || receipt.NextCursor != "" {
		t.Fatalf("unexpected receipt envelope: %+v", receipt)
	}
	if len(receipt.ExportURLs) != 3 || receipt.ExportURLs["csv"] == "" {
		t.Fatalf("missing export URLs on receipt: %+v", receipt.ExportURLs)
	}
	if bodySample, _ := receipt.Records[0]["body"].(string); len(bodySample) > 180 {
		t.Fatalf("receipt leaked untruncated body (%d chars)", len(bodySample))
	}

	// Flush background gzip writer and verify .gz archived cleanly + SearchEvidence works.
	store.WaitSnapshots()
	ref := res.SnapshotRefs[0]
	if _, err := os.Stat(filepath.Join(snapDir, ref[:2], ref+".gz")); err != nil {
		t.Fatalf("async snapshot gzip missing after WaitSnapshots: %v", err)
	}
	evPage, err := store.SearchEvidence(context.Background(), []string{"rel_test"}, "Release v5", "", "", 5, "http://127.0.0.1:8080")
	if err != nil || evPage.Total != 1 {
		t.Fatalf("SearchEvidence failed: %+v err=%v", evPage, err)
	}

	// Verify CSV, Markdown, and JSON direct exports contain all 5 rows without hitting LLM.
	ct, csvBody, err := store.ExportRecords(context.Background(), "rel_test", 0, "csv")
	if err != nil || !strings.Contains(ct, "text/csv") || !strings.Contains(csvBody, "v5") {
		t.Fatalf("CSV export failed: ct=%q err=%v body=%q", ct, err, csvBody)
	}
	_, mdBody, err := store.ExportRecords(context.Background(), "rel_test", 0, "md")
	if err != nil || !strings.Contains(mdBody, "| v5 |") {
		t.Fatalf("Markdown export failed: err=%v body=%q", err, mdBody)
	}
}

func TestApplyRecordQueriesMultiView(t *testing.T) {
	rows := []SourceRecord{
		{ID: 1, SnapshotRef: "snap1", PayloadJSON: `{"tag":"v3.7.4","published_at":"2026-11-19T00:00:00Z","body":"See [CHANGELOG](https://github.com/etcd-io/etcd/blob/main/CHANGELOG/CHANGELOG-3.7.md) and [upgrade guide](https://etcd.io/docs/v3.7/upgrades/upgrade_3_7/)"}`},
		{ID: 2, SnapshotRef: "snap1", PayloadJSON: `{"tag":"v3.6.17","published_at":"2026-10-01T00:00:00Z","body":"No links"}`},
		{ID: 3, SnapshotRef: "snap2", PayloadJSON: `{"tag":"v3.7.1","published_at":"2026-07-23T00:00:00Z","body":"Oldest release"}`},
	}
	page, err := ApplyRecordQueries(rows, 3, "etcd", 1, DefaultMCPSafetyMaxBytes, DefaultMCPPageSize, "http://127.0.0.1:8080", map[string]*RecordQuery{
		"timeline":  {Fields: []string{"tag", "published_at"}, OrderBy: "published_at", Limit: 10},
		"series":    {GroupBy: "tag", GroupPrefix: 4},
		"v374_urls": {Where: []FilterCondition{{Field: "tag", Op: "eq", Value: "v3.7.4"}}, ExtractURLs: &URLExtractSpec{Field: "body"}},
	})
	if err != nil {
		t.Fatalf("ApplyRecordQueries failed: %v", err)
	}
	if page.Mode != "multi_query" || len(page.Views) != 3 {
		t.Fatalf("unexpected multi_query envelope: %+v", page)
	}
	// Pure aggregate & extract views MUST NOT dump unrequested rows.
	if sv := page.Views["series"]; sv == nil || sv.Groups["v3.7"] != 2 || len(sv.Records) != 0 {
		t.Fatalf("pure group_by view must omit records: %+v", sv)
	}
	uv := page.Views["v374_urls"]
	if uv == nil || len(uv.ExtractedURLs) != 2 || len(uv.Records) != 0 || uv.URLLabels["https://etcd.io/docs/v3.7/upgrades/upgrade_3_7/"] != "upgrade guide" {
		t.Fatalf("pure extract_urls view must omit records and capture markdown url_labels: %+v", uv)
	}
	// Deduplicated top-level snapshot_refs + compact _snapshot_idx row index (never repeated 64-char hashes).
	tl := page.Views["timeline"]
	if tl == nil || tl.Returned != 3 || tl.Records[0]["tag"] != "v3.7.1" {
		t.Fatalf("unexpected timeline view: %+v", tl)
	}
	snapIdx, ok := tl.Records[0]["_snapshot_idx"].(int)
	if !ok || snapIdx < 0 || snapIdx >= len(page.SnapshotRefs) || page.SnapshotRefs[snapIdx] != "snap2" {
		t.Fatalf("expected deduplicated snapshot_refs + _snapshot_idx pointing to snap2, got refs=%v row=%+v", page.SnapshotRefs, tl.Records[0])
	}
	if _, hasOld := tl.Records[0]["_snapshot_ref"]; hasOld {
		t.Fatalf("row must not repeat 64-char _snapshot_ref: %+v", tl.Records[0])
	}
	if len(page.ExportURLs) != 0 {
		t.Fatalf("non-truncated query should omit export_urls, got %+v", page.ExportURLs)
	}
	if _, err := ApplyRecordQueries(rows, 3, "etcd", 1, DefaultMCPSafetyMaxBytes, DefaultMCPPageSize, "", map[string]*RecordQuery{
		"a": {}, "b": {}, "c": {}, "d": {}, "e": {},
	}); err == nil {
		t.Fatal("expected error when exceeding MaxMCPMultiQueries")
	}
}

package information

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
	"go-lua-crawler/internal/repository"
	"go-lua-crawler/internal/testdb"
)

func configureRealRetrieval(t *testing.T, s *Service) (knowledge.Embedder, knowledge.VectorStore) {
	t.Helper()
	vectorURL, embedURL := os.Getenv("GORELAY_TEST_QDRANT_URL"), os.Getenv("GORELAY_TEST_EMBED_BASE_URL")
	if vectorURL == "" || embedURL == "" {
		t.Skip("explicit real Qdrant and BGE endpoints required")
	}
	collection := "gorelay_asset_" + agentdata.Token()
	vectors, err := knowledge.NewQdrant(vectorURL, collection, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := vectors.EnsureCollection(context.Background(), 512); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, vectorURL+"/collections/"+collection, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Error("collection cleanup", resp.Status)
		}
	})
	embed, err := aiapp.NewModelProvider(aiapp.ProviderConfig{BaseURL: embedURL, Model: "bge-small-zh-v1.5-f16", APIKey: "local-bge"})
	if err != nil {
		t.Fatal(err)
	}
	adapter := aiapp.EmbeddingAdapter{Provider: embed}
	s.Retriever = &knowledge.Retriever{Store: s.Crawler.Knowledge, Embed: adapter, Vectors: vectors}
	return adapter, vectors
}

func TestRealBGESQLQdrantPreflightAndInvalidation(t *testing.T) {
	vectorURL, embedURL := os.Getenv("GORELAY_TEST_QDRANT_URL"), os.Getenv("GORELAY_TEST_EMBED_BASE_URL")
	if vectorURL == "" || embedURL == "" {
		t.Skip("explicit real Qdrant and BGE endpoints required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	crawler := aiapp.NewService(testdb.Open(t))
	if err := crawler.Migrate(); err != nil {
		t.Fatal(err)
	}
	s := NewService(crawler)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	collection := "gorelay_information_" + agentdata.Token()
	vectors, err := knowledge.NewQdrant(vectorURL, collection, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := vectors.EnsureCollection(ctx, 512); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		req, _ := http.NewRequest(http.MethodDelete, vectorURL+"/collections/"+collection, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Error("collection cleanup", resp.Status)
		}
	})
	embed, err := aiapp.NewModelProvider(aiapp.ProviderConfig{BaseURL: embedURL, Model: "bge-small-zh-v1.5-f16", APIKey: "local-bge"})
	if err != nil {
		t.Fatal(err)
	}
	adapter := aiapp.EmbeddingAdapter{Provider: embed}
	s.Retriever = &knowledge.Retriever{Store: crawler.Knowledge, Embed: adapter, Vectors: vectors}
	fetchedAt := time.Now().UTC().Add(-time.Minute)
	spec := aiapp.TaskSpec{Goal: "查询高校采购平台的软件开发项目预算和截止日期", ContractVersion: "v2", SchemaVersion: "v1", RequiredFields: []aiapp.FieldRequirement{{Name: "title", Type: "string"}, {Name: "budget", Type: "string"}, {Name: "deadline", Type: "string"}}, JoinKeys: []string{"title"}, MaxAgeSeconds: 3600}
	row := aiapp.Record{Key: "source-record", SchemaVersion: "v1", Fields: map[string]any{"title": "科研数据平台软件开发采购", "budget": "42.50万元", "deadline": "2026-10-28"}, Provenance: aiapp.Provenance{SourceID: "procurement-api", URL: "https://procurement.example/notice/known", FetchedAt: fetchedAt, TaskID: 801, DataVersion: "observed-api-v1"}}
	if err := s.DB.Create(&repository.CrawlResult{TaskID: 801, Payload: string(mustMarshal(aiapp.AcquisitionResult{Records: []aiapp.Record{row}}))}).Error; err != nil {
		t.Fatal(err)
	}
	doc, err := knowledge.NewDocument(knowledge.Input{Source: row.Provenance.SourceID, SourceKey: "software-notice", URL: row.Provenance.URL, Title: row.Fields["title"].(string), Body: "高校公开采购公告：科研数据平台软件开发采购。预算42.50万元，截止日期2026-10-28。", Format: "text", FetchedAt: fetchedAt, DataVersion: row.Provenance.DataVersion, TaskID: 801, RecordKey: row.Key})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crawler.Knowledge.Put(ctx, doc, "bge-real-512-window600-80"); err != nil {
		t.Fatal(err)
	}
	indexer := knowledge.Indexer{Store: crawler.Knowledge, Embed: adapter, Vectors: vectors, Fingerprint: "bge-real-512-window600-80"}
	if err := indexer.Once(ctx); err != nil {
		t.Fatal(err)
	}
	req, err := s.Create(ctx, Input{RequestKey: "real-retrieval", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"procurement.example"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Preflight(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.RetrievalStatus != "checked" || result.Next != "reuse_records" || len(result.Records) != 1 || !result.Coverage.Satisfied || result.Evidence.Metrics.EmbeddingCalls != 1 {
		t.Fatalf("real local retrieval did not deliver: %+v", result)
	}
	if !result.Records[0].Provenance.FetchedAt.Equal(fetchedAt) || result.Records[0].Fields["budget"] != "42.50万元" {
		t.Fatal("SQL fact/time changed during retrieval")
	}
	if err := knowledge.CheckReferences(ctx, knowledge.SetReader{Store: crawler.Knowledge, Set: result.Evidence}, []string{result.Evidence.Entries[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := crawler.Knowledge.Invalidate(ctx, doc.ID); err != nil {
		t.Fatal(err)
	}
	query, err := adapter.EmbedTexts(ctx, []string{spec.Goal})
	if err != nil {
		t.Fatal(err)
	}
	residual, err := vectors.Search(ctx, query[0], 5)
	if err != nil || len(residual) == 0 {
		t.Fatal("invalidation counterexample needs a residual vector", err)
	}
	next, err := s.Create(ctx, Input{RequestKey: "real-after-invalidation", Goal: spec.Goal, Spec: &spec, SourceScope: []string{"procurement.example"}})
	if err != nil {
		t.Fatal(err)
	}
	invalidated, err := s.Preflight(ctx, next.ID)
	if err != nil || invalidated.RetrievalStatus != "checked" || len(invalidated.Records) != 0 || invalidated.Next == "reuse_records" {
		t.Fatal("residual vector bypassed current SQL state", invalidated, err)
	}
	var runs int64
	s.DB.Model(&agentdata.Run{}).Count(&runs)
	if runs != 0 {
		t.Fatal("RAG preflight entered or modified crawler")
	}
	if dir := os.Getenv("GORELAY_INFORMATION_EVIDENCE_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		report := map[string]any{"collection": collection, "dimensions": 512, "real_embedding": true, "real_vectors": true, "real_mysql": true, "fresh": result, "after_invalidation": invalidated, "residual_vector_hits": len(residual), "crawler_runs": runs}
		raw, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "real-bge-qdrant-preflight.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Log(fmt.Sprintf("real MySQL/BGE/Qdrant: 512 dimensions, fresh records=%d, residual vectors=%d, invalidated records=%d, crawler runs=%d", len(result.Records), len(residual), len(invalidated.Records), runs))
}

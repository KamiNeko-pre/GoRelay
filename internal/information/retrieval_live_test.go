package information

import (
	"context"
	"net/http"
	"os"
	"testing"

	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/aiapp"
	"go-lua-crawler/internal/knowledge"
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

package knowledge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testQdrantProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("api-key") != "test" {
			t.Error("missing key")
		}
		switch r.URL.Path {
		case "/collections/knowledge/points":
			if r.Method != "PUT" || r.URL.Query().Get("wait") != "true" {
				t.Error("write acknowledgment is not awaited")
			}
			w.Write([]byte(`{"status":"ok","result":{"status":"completed"}}`))
		case "/collections/knowledge/points/query":
			w.Write([]byte(`{"status":"ok","result":{"points":[{"id":"00000000-0000-0000-0000-000000000001","score":0.9}]}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	client, err := NewQdrant(server.URL, "knowledge", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err = client.Upsert(context.Background(), []VectorPoint{{ID: "00000000-0000-0000-0000-000000000001", Vector: []float32{1, 0}}}); err != nil {
		t.Fatal(err)
	}
	hits, err := client.Search(context.Background(), []float32{1, 0}, 3)
	if err != nil || len(hits) != 1 || hits[0].Score != 0.9 {
		t.Fatalf("bad query: %+v %v", hits, err)
	}
}

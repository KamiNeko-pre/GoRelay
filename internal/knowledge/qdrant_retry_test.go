package knowledge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVectorRetryAfterDoesNotBlockBeyondCallTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(503)
	}))
	defer server.Close()
	q, _ := NewQdrant(server.URL, "case", "")
	q.client.Timeout = 100 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- q.Upsert(ctx, nil) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("failure hidden")
		}
	case <-time.After(300 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("retry slept beyond existing call timeout")
	}
}

func TestIdempotentVectorUpsertRecoversLostAckWithoutChangingPoints(t *testing.T) {
	for _, fault := range []string{"lost_connection", "service_unavailable"} {
		t.Run(fault, func(t *testing.T) {
			bodies := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				bodies = append(bodies, string(body))
				if len(bodies) == 1 {
					if fault == "lost_connection" {
						conn, _, _ := w.(http.Hijacker).Hijack()
						conn.Close()
						return
					}
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(503)
					return
				}
				w.Write([]byte(`{"result":{"status":"completed"}}`))
			}))
			defer server.Close()
			q, _ := NewQdrant(server.URL, "case", "")
			err := q.Upsert(context.Background(), []VectorPoint{{ID: "stable-point", Vector: []float32{1, 2}, Payload: map[string]any{"generation": "frozen"}}})
			if err != nil || len(bodies) != 2 || bodies[0] != bodies[1] {
				t.Fatalf("idempotent recovery failed: %v requests=%d", err, len(bodies))
			}
		})
	}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(400) }))
	defer server.Close()
	q, _ := NewQdrant(server.URL, "case", "")
	if err := q.Upsert(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "400") || calls != 1 {
		t.Fatal("permanent input failure retried or hidden")
	}
}

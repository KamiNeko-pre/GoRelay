package aiapp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReceivedMalformedHTTPJSONIsKnownProtocolFailure(t *testing.T) {
	for _, body := range []string{`{broken`, `{"choices":"wrong type"}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }))
			defer server.Close()
			p, _ := NewModelProvider(ProviderConfig{BaseURL: server.URL, APIKey: "fixture", Model: "fixture"})
			_, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "fixture"}}})
			if !errors.Is(err, ErrInvalidProposal) {
				t.Fatalf("fully received malformed response confused with unknown transport: %v", err)
			}
		})
	}
}

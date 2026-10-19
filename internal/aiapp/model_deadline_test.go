package aiapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestChatDefaultTransportDoesNotShortenTaskDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(60 * time.Millisecond):
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant", Content: "complete"}, "finish_reason": "stop"}}})
	}))
	defer server.Close()
	for _, explicit := range []bool{false, true} {
		p, err := NewModelProvider(ProviderConfig{BaseURL: server.URL, Model: "fixture", APIKey: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		// Scale the old 45s transport default to 10ms. The contract is which
		// owner sets the deadline, not how long a unit test can afford to sleep.
		p.client.Timeout = 10 * time.Millisecond
		if explicit {
			p.config.Timeout = p.client.Timeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		out, err := p.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "perform task"}}, MaxCompletionTokens: 32})
		cancel()
		if !explicit && (err != nil || out.Message.Content != "complete") {
			t.Fatal("implicit HTTP timeout interrupted healthy task", err)
		}
		if explicit && err == nil {
			t.Fatal("explicit caller timeout ignored")
		}
	}
	p, _ := NewModelProvider(ProviderConfig{BaseURL: server.URL, Model: "fixture", APIKey: "fixture"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := p.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "task"}}}); err == nil {
		t.Fatal("task deadline ignored")
	}
}

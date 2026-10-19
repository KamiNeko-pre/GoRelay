package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestChatWithoutTaskTimeboxDoesNotCreateExpiredDeadline(t *testing.T) {
	for _, setting := range []string{"", "0s"} {
		t.Run("timeout="+setting, func(t *testing.T) {
			t.Setenv("GORELAY_RUN_TIMEOUT", setting)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": Message{Role: "assistant", Content: "complete"}, "finish_reason": "stop"}}, "usage": &Usage{TotalTokens: 3}})
			}))
			defer server.Close()
			provider, err := NewModelProvider(ProviderConfig{BaseURL: server.URL, Model: "fixture", APIKey: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			result, err := provider.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "collect current facts"}}})
			if err != nil || result.Message.Content != "complete" || calls.Load() != 1 || result.Usage == nil || result.Usage.TotalTokens != 3 {
				t.Fatalf("zero task timebox became an expired provider deadline: result=%+v calls=%d err=%v", result, calls.Load(), err)
			}
		})
	}
}

func TestChatPositiveConfiguredTimeboxStillCancelsUnboundedCaller(t *testing.T) {
	t.Setenv("GORELAY_RUN_TIMEOUT", "10ms")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Second):
			w.Write([]byte("{}"))
		}
	}))
	defer server.Close()
	provider, err := NewModelProvider(ProviderConfig{BaseURL: server.URL, Model: "fixture", APIKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "collect other facts"}}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("explicit task timebox was lost: %v", err)
	}
}

func TestChatInvalidConfiguredTimeboxFailsBeforeDispatch(t *testing.T) {
	for _, setting := range []string{"invalid", "-1s"} {
		t.Run(setting, func(t *testing.T) {
			t.Setenv("GORELAY_RUN_TIMEOUT", setting)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
			defer server.Close()
			provider, err := NewModelProvider(ProviderConfig{BaseURL: server.URL, Model: "fixture", APIKey: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "validate configuration"}}})
			if err == nil || calls.Load() != 0 {
				t.Fatalf("invalid timebox dispatched provider request: calls=%d err=%v", calls.Load(), err)
			}
		})
	}
}

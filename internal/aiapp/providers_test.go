package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelNativeToolsAndUsage(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("wrong protocol")
		}
		var in ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		if in.Model != "test-model" || len(in.Tools) != 1 || in.Messages[1].ToolCallID != "previous" {
			t.Errorf("lost tool protocol: %+v", in)
		}
		w.Header().Set("X-Request-ID", "receipt-1")
		w.Write([]byte(`{"model":"resolved-model","choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"discover_sources","arguments":"{\"query\":\"release notes\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`))
	}))
	defer s.Close()
	p, err := NewModelProvider(ProviderConfig{BaseURL: s.URL + "/v1", APIKey: "test-key", Model: "test-model"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "collect versions"}, {Role: "tool", ToolCallID: "previous", Content: "not found"}}, Tools: []ToolDefinition{{Type: "function", Function: FunctionDefinition{Name: "discover_sources", Parameters: json.RawMessage(`{"type":"object"}`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Message.ToolCalls[0].Function.Name != "discover_sources" || got.Usage == nil || got.Usage.TotalTokens != 30 || got.RequestID != "receipt-1" || got.Model != "resolved-model" {
		t.Fatalf("bad response: %+v", got)
	}
}

func TestProviderNoSilentPaidRetryOrUsageZero(t *testing.T) {
	calls := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":"secret echo"}`))
			return
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer s.Close()
	p, _ := NewModelProvider(ProviderConfig{BaseURL: s.URL, APIKey: "test-key", Model: "m"})
	_, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	var remote *ProviderError
	if !errors.As(err, &remote) || !remote.Retryable || remote.RetryAfter != "2" || calls != 1 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("bad error/retry: %v %d", err, calls)
	}
	got, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil || got.Usage != nil {
		t.Fatalf("missing usage must stay unknown: %+v %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = p.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel not propagated: %v", err)
	}
}

func TestProviderDistinguishesQuotaFromTemporary429(t *testing.T) {
	for _, code := range []string{"insufficient_quota", "credit_balance_exhausted", "project_spend_limit_exceeded", "organization_spend_limit_exceeded", "organization_usage_limit_exceeded", "slow_down"} {
		t.Run(code, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(429)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": "private prompt echo"}})
			}))
			defer s.Close()
			p, _ := NewModelProvider(ProviderConfig{BaseURL: s.URL, APIKey: "fixture", Model: "fixture"})
			_, err := p.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Content: "request"}}})
			if errors.Is(err, ErrProviderQuota) != (code != "slow_down") {
				t.Fatalf("wrong 429 classification: %v", err)
			}
			if strings.Contains(err.Error(), "private") {
				t.Fatal("provider error body leaked")
			}
		})
	}
}

func TestEmbeddingOrderingAndInvalidBatch(t *testing.T) {
	broken := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path=%s", r.URL.Path)
		}
		if broken {
			w.Write([]byte(`{"data":[{"index":0,"embedding":[1,2]},{"index":0,"embedding":[3,4]}]}`))
			return
		}
		w.Write([]byte(`{"model":"embedding-test","data":[{"index":1,"embedding":[3,4]},{"index":0,"embedding":[1,2]}],"usage":{"prompt_tokens":4,"total_tokens":4}}`))
	}))
	defer s.Close()
	p, _ := NewModelProvider(ProviderConfig{BaseURL: s.URL, APIKey: "k", Model: "embedding-test"})
	got, err := p.Embed(context.Background(), []string{"first", "second"})
	if err != nil || len(got.Vectors) != 2 || got.Vectors[0][0] != 1 {
		t.Fatalf("wrong alignment: %+v %v", got, err)
	}
	broken = true
	if _, err = p.Embed(context.Background(), []string{"first", "second"}); err == nil {
		t.Fatal("duplicate embedding indices accepted")
	}
}

func TestSearchCandidatesAreBoundedAndNotFacts(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "component release" || r.Header.Get("X-Subscription-Token") != "search-key" {
			t.Error("wrong search protocol")
		}
		w.Write([]byte(`{"web":{"results":[{"title":"A","url":"https://example.com/a","description":"search snippet"},{"title":"duplicate","url":"https://example.com/a"},{"url":"javascript:alert(1)"},{"title":"B","url":"https://example.org/b"}]}}`))
	}))
	defer s.Close()
	p, err := NewBraveSearch(SearchConfig{Endpoint: s.URL, APIKey: "search-key"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Search(context.Background(), "component release", 2)
	if err != nil || len(got.Candidates) != 2 || got.Candidates[0].Discovery != "brave_search" {
		t.Fatalf("bad candidates: %+v %v", got, err)
	}
	if _, err = NewBraveSearch(SearchConfig{}); err == nil {
		t.Fatal("missing key accepted")
	}
}

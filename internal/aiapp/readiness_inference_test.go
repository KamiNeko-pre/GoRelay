package aiapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPReadinessChecksInferenceRatherThanCatalog(t *testing.T) {
	for _, tc := range []struct {
		name          string
		catalog, chat int
		body          string
		ready         bool
	}{
		{"missing_catalog_valid_inference", 404, 200, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"probe","type":"function","function":{"name":"gorelay_readiness","arguments":"{\"ready\":true}"}}]},"finish_reason":"stop"}],"usage":{"total_tokens":3}}`, true},
		{"catalog_ok_chat_forbidden", 200, 403, `{}`, false},
		{"catalog_ok_chat_overloaded", 200, 503, `{}`, false},
		{"healthy_catalog_and_inference", 200, 200, `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"probe","type":"function","function":{"name":"gorelay_readiness","arguments":"{\"ready\":true}"}}]},"finish_reason":"stop"}]}`, true},
		{"empty_inference", 200, 200, `{"choices":[{"message":{"role":"assistant","content":""}}]}`, false},
		{"malformed_inference", 200, 200, `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inferenceCalls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/models" {
					w.WriteHeader(tc.catalog)
					_, _ = w.Write([]byte(`{"data":[{"id":"fixture"}]}`))
					return
				}
				if r.URL.Path != "/chat/completions" || r.Method != http.MethodPost {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
					return
				}
				inferenceCalls++
				var request ChatRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if request.Model != "fixture" || len(request.Tools) != 1 || request.Tools[0].Function.Name != "gorelay_readiness" || request.MaxCompletionTokens <= 0 || request.MaxCompletionTokens > 512 {
					t.Errorf("unbounded or non-isolated probe: %+v", request)
				}
				// The probe must stay a minimal, side-effect-free conversation, but it may
				// carry a system turn: providers that require a leading system
				// message would otherwise report the backend unavailable while every
				// real run works.
				if len(request.Messages) < 1 || len(request.Messages) > 2 {
					t.Errorf("probe message shape is not minimal: %+v", request.Messages)
				}
				if request.Messages[len(request.Messages)-1].Role != "user" {
					t.Errorf("probe must end with the user turn: %+v", request.Messages)
				}
				w.WriteHeader(tc.chat)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p, err := NewModelProvider(ProviderConfig{BaseURL: srv.URL, APIKey: "fixture", Model: "fixture", Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			result, err := p.Preflight(context.Background())
			if result.Ready != tc.ready || (err == nil) != tc.ready || inferenceCalls != 1 || result.QuotaVerified {
				t.Fatalf("wrong readiness boundary: %+v err=%v chat=%d", result, err, inferenceCalls)
			}
			if tc.name == "missing_catalog_valid_inference" && (result.Usage == nil || result.Usage.TotalTokens != 3) {
				t.Fatalf("probe usage lost: %+v", result)
			}
			if tc.name == "healthy_catalog_and_inference" && result.Usage != nil {
				t.Fatalf("missing usage invented: %+v", result)
			}
		})
	}
}

func TestInferenceReadinessIsSharedAcrossConcurrentSubmissions(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"probe","type":"function","function":{"name":"gorelay_readiness","arguments":"{\"ready\":true}"}}]},"finish_reason":"stop"}],"usage":{"total_tokens":3}}`))
	}))
	defer srv.Close()
	p, err := NewModelProvider(ProviderConfig{BaseURL: srv.URL, APIKey: "fixture", Model: "fixture", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(nil)
	s.Model = p
	s.readiness = &modelReadinessState{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.PreflightModel(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("concurrent probe multiplied inference cost: %d", calls.Load())
	}
	status := s.ModelReadiness()
	if !status.Ready || status.Usage == nil || status.Usage.TotalTokens != 3 || status.QuotaVerified || time.Until(s.readiness.until) < 9*time.Minute {
		t.Fatalf("probe status or cache incorrect: %+v", status)
	}
}

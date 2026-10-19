package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/testdb"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type codexRefusalChat struct {
	calls int
	raw   string
}

func (p *codexRefusalChat) Chat(_ context.Context, request ChatRequest) (ChatResponse, error) {
	p.calls++
	response, err := parseCodexInference(strings.NewReader(p.raw), request, "test-model")
	return response, codexResultError(err, errors.New("Codex process exit 1"))
}

func TestCodexAuthRefusalStopsAndRemainsStoppedAfterServiceRecovery(t *testing.T) {
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			p := &codexRefusalChat{raw: fmt.Sprintf(`{"type":"turn.failed","error":{"message":"provider request rejected (%d)"}}`, status)}
			s.Model = p
			r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "codex-auth", Goal: "collect public facts"})
			if err != nil {
				t.Fatal(err)
			}
			_ = s.Advance(context.Background(), r.ID)
			stored, err := s.Data.Run(context.Background(), r.ID)
			if err != nil || stored.Status != "failed" || p.calls != 1 {
				t.Fatalf("refusal retried: %+v calls=%d err=%v", stored, p.calls, err)
			}
			recovered := NewService(s.Data.DB)
			recovered.Model = p
			for n := 0; n < 3; n++ {
				_ = recovered.Advance(context.Background(), r.ID)
			}
			if p.calls != 1 {
				t.Fatal("recovered service dispatched revoked credentials again")
			}
		})
	}
}

type quotaChat struct{ calls int }

func (p *quotaChat) Chat(context.Context, ChatRequest) (ChatResponse, error) {
	p.calls++
	return ChatResponse{}, ErrProviderQuota
}

func TestQuotaStopsRunWithoutThreeMoreInferenceAttempts(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := &quotaChat{}
	s.Model = p
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "quota", Goal: "collect public evidence"})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Advance(context.Background(), r.ID)
	stored, err := s.Data.Run(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.IsTerminal() || stored.Status != "failed" || p.calls != 1 || stored.LastError != ErrProviderQuota.Error() {
		t.Fatalf("quota kept looping: %+v %d", stored, p.calls)
	}
	for n := 0; n < 3; n++ {
		_ = s.Advance(context.Background(), r.ID)
	}
	if p.calls != 1 {
		t.Fatal("terminal quota run dispatched more inference")
	}
}

type measuredFailedSearch struct{}

func (measuredFailedSearch) Search(context.Context, string, int) (SearchResponse, error) {
	return SearchResponse{Usage: &Usage{PromptTokens: 11, TotalTokens: 11}}, errors.New("response format rejected")
}

func TestFailedSearchKeepsKnownUsageInOriginalReceipt(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	s.Search = measuredFailedSearch{}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{RequestKey: "search-cost", Goal: "discover public sources"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.executeDurableTool(ctx, r, ToolCall{ID: "search", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"public metadata","count":3}`}})
	if err == nil {
		t.Fatal("failed search accepted")
	}
	ops, err := s.Data.Operations(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	metrics := OperationMetrics(ops)
	if metrics["reported_total_tokens"] != int64(11) {
		raw, _ := json.Marshal(ops)
		t.Fatalf("failed search lost measured usage: %s", raw)
	}
}

func TestPermanentProviderFailureStopsImmediately(t *testing.T) {
	for _, status := range []int{401, 402, 403} {
		t.Run(fmt.Sprintf("HTTP%d", status), func(t *testing.T) {
			calls := 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(status)
				w.Write([]byte(`{"error":{"message":"private echo"}}`))
			}))
			defer remote.Close()
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			s.Model, _ = NewModelProvider(ProviderConfig{BaseURL: remote.URL, APIKey: "fixture", Model: "fixture"})
			r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "permanent", Goal: "collect public facts"})
			if err != nil {
				t.Fatal(err)
			}
			for n := 0; n < 4; n++ {
				_ = s.Advance(context.Background(), r.ID)
			}
			stored, err := s.Data.Run(context.Background(), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !stored.IsTerminal() || calls != 1 {
				t.Fatalf("permanent rejection retried: status=%s calls=%d", stored.Status, calls)
			}
		})
	}
}

func TestProviderRetryAfterPreventsImmediateRetry(t *testing.T) {
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(429)
	}))
	defer remote.Close()
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	s.Model, _ = NewModelProvider(ProviderConfig{BaseURL: remote.URL, APIKey: "fixture", Model: "fixture"})
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "retry-after", Goal: "collect public facts"})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 3; n++ {
		_ = s.Advance(context.Background(), r.ID)
	}
	if calls != 1 {
		t.Fatalf("Retry-After ignored: %d requests", calls)
	}
	// Recreating the service must retain the not-before time and reservation.
	restored := NewService(s.Data.DB)
	restored.Model = s.Model
	_ = restored.Advance(context.Background(), r.ID)
	if calls != 1 {
		t.Fatal("restart bypassed Retry-After")
	}
	stored, err := s.Data.Run(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var budget BudgetSnapshot
	json.Unmarshal([]byte(stored.BudgetJSON), &budget)
	if budget.Spent.ModelCalls != 1 || budget.RetryNotBefore == nil {
		t.Fatalf("retry timing/accounting not durable: %+v", budget)
	}
	// Expire only the fixture's backoff to prove a new attempt becomes possible.
	past := time.Now().Add(-time.Second)
	budget.RetryNotBefore = &past
	raw, _ := json.Marshal(budget)
	if err = s.Data.DB.Model(&stored).Update("budget_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	// The original failure receipt is also an authoritative recovery boundary.
	ops, err := s.Data.Operations(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Kind == "model" {
			var response ChatResponse
			json.Unmarshal([]byte(op.ResultJSON), &response)
			if response.Failure != nil {
				response.Failure.RetryAt = &past
				raw, _ := json.Marshal(response)
				if err = s.Data.DB.Model(&op).Update("result_json", string(raw)).Error; err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	_ = restored.Advance(context.Background(), r.ID)
	if calls != 2 {
		t.Fatal("temporary error could never recover")
	}
}

type deadlineChat struct{ deadline time.Time }

func (p *deadlineChat) Chat(ctx context.Context, in ChatRequest) (ChatResponse, error) {
	p.deadline, _ = ctx.Deadline()
	return ChatResponse{Message: Message{Role: "assistant", Content: "fixture"}}, nil
}

func TestModelAttemptReceivesRemainingRunDeadline(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := &deadlineChat{}
	s.Model = p
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "deadline", Goal: "public metadata"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Advance(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	if p.deadline.IsZero() || p.deadline.Sub(r.Deadline) > time.Millisecond || r.Deadline.Sub(p.deadline) > time.Millisecond {
		t.Fatalf("request deadline differs from run deadline: %v %v", p.deadline, r.Deadline)
	}
}

func TestProviderRetryAfterParsesSecondsAndHTTPDate(t *testing.T) {
	state := decisionState()
	state.Budget.LoopVersion = 2
	now := time.Now()
	for _, header := range []string{"120", now.Add(2 * time.Minute).UTC().Format(http.TimeFormat)} {
		when := providerRetryTime(&ProviderError{Status: 429, Retryable: true, RetryAfter: header}, 0, now)
		if when.Before(now.Add(119 * time.Second)) {
			t.Fatal("Retry-After shortened", header)
		}
	}
}

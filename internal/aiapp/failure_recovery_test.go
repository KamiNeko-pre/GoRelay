package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Reproduce the real boundary: operation receipt commits, then the Run
// transition fails. No tool, model result or accounting row is hand-filled.
func crashRunTransition(t *testing.T, db *gorm.DB) {
	t.Helper()
	fired := false
	err := db.Callback().Update().Before("gorm:update").Register("fixture:crash-run-transition", func(tx *gorm.DB) {
		fields, ok := tx.Statement.Dest.(map[string]interface{})
		if tx.Statement.Table == "agent_runs" && ok && fields["status"] != nil && !fired {
			fired = true
			tx.AddError(errors.New("fixture crash after operation receipt"))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}
func releaseCrashedLease(t *testing.T, s *Service, id string) {
	t.Helper()
	if err := s.Data.DB.Model(&agentdata.Run{}).Where("id = ?", id).Update("lease_until", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
}

func TestFailedModelReceiptSurvivesCrashWithoutRetryingPermanentError(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Retry-After", "300")
				w.WriteHeader(status)
			}))
			defer remote.Close()
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			s.Model, _ = NewModelProvider(ProviderConfig{BaseURL: remote.URL, APIKey: "fixture", Model: "fixture"})
			r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "model-crash", Goal: "public evidence"})
			if err != nil {
				t.Fatal(err)
			}
			crashRunTransition(t, s.Data.DB)
			if err = s.Advance(context.Background(), r.ID); err == nil {
				t.Fatal("crash did not happen")
			}
			releaseCrashedLease(t, s, r.ID)
			restored := NewService(s.Data.DB)
			restored.Model = s.Model
			_ = restored.Advance(context.Background(), r.ID)
			stored, err := s.Data.Run(context.Background(), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("restored failure made another external call: %d", calls)
			}
			if status != 429 && !stored.IsTerminal() {
				t.Fatal("permanent failure lost its stop semantics")
			}
			if status == 429 && stored.IsTerminal() {
				t.Fatal("temporary rate limit became a permanent failure")
			}
		})
	}
}

func TestFailedToolReceiptSurvivesCrashWithOriginalClassification(t *testing.T) {
	for _, status := range []int{401, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Retry-After", "300")
				w.WriteHeader(status)
			}))
			defer remote.Close()
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			s.Search, _ = NewBraveSearch(SearchConfig{Endpoint: remote.URL, APIKey: "fixture"})
			r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "tool-crash", Goal: "discover public evidence"})
			if err != nil {
				t.Fatal(err)
			}
			call := ToolCall{ID: "original-search", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"public evidence"}`}}
			raw, _ := json.Marshal([]Message{{Role: "assistant", ToolCalls: []ToolCall{call}}})
			if err = s.Data.DB.Model(&r).Update("plan_json", string(raw)).Error; err != nil {
				t.Fatal(err)
			}
			crashRunTransition(t, s.Data.DB)
			if err = s.Advance(context.Background(), r.ID); err == nil {
				t.Fatal("crash did not happen")
			}
			releaseCrashedLease(t, s, r.ID)
			restored := NewService(s.Data.DB)
			restored.Search = s.Search
			_ = restored.Advance(context.Background(), r.ID)
			stored, err := s.Data.Run(context.Background(), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("tool receipt replay repeated the search")
			}
			if status == 401 && !stored.IsTerminal() {
				t.Fatal("replayed authentication failure was reduced to plain text")
			}
			if status == 429 && stored.IsTerminal() {
				t.Fatal("temporary failure was not protected")
			}
		})
	}
}

func TestToolRetryAfterBlocksFreshRequestButKeepsReadsAvailable(t *testing.T) {
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "300")
		w.WriteHeader(429)
	}))
	defer remote.Close()
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	s.Search, _ = NewBraveSearch(SearchConfig{Endpoint: remote.URL, APIKey: "fixture"})
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "tool-backoff", Goal: "public evidence"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "first", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"public evidence"}`}}
	_, _ = s.executeDurableTool(context.Background(), r, call)
	call.ID = "changed-query"
	call.Function.Arguments = `{"query":"a different query"}`
	_, err = s.executeDurableTool(context.Background(), r, call)
	if calls != 1 {
		t.Fatal("new query bypassed provider Retry-After", calls)
	}
	if err == nil {
		t.Fatal("cooldown not returned as feedback")
	}
	if _, err = s.executeDurableTool(context.Background(), r, ToolCall{ID: "read", Type: "function", Function: FunctionCall{Name: "read_task_context", Arguments: `{}`}}); err != nil {
		t.Fatal("local state read blocked by search outage", err)
	}
}

func TestPendingReceiptReplayDoesNotNeedAnotherToolReservation(t *testing.T) {
	state := decisionState()
	state.Budget.Limits.ToolCalls = 1
	state.Budget.Spent.ToolCalls = 1
	call := ToolCall{ID: "committed", Type: "function", Function: FunctionCall{Name: "list_sources", Arguments: `{}`}}
	state.Messages = []Message{{Role: "assistant", ToolCalls: []ToolCall{call}}}
	state.Operations = []agentdata.Operation{{Key: "tool:committed", Kind: "list_sources", Stage: "succeeded"}}
	action, err := RunTaskLoop(context.Background(), state)
	if err != nil || action.Kind != "tools" {
		t.Fatal("replay rejected as another tool expense", action, err)
	}
}

func TestPermanentProviderFailureStopsRemainingBatchCalls(t *testing.T) {
	calls := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(401) }))
	defer remote.Close()
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	s.Search, _ = NewBraveSearch(SearchConfig{Endpoint: remote.URL, APIKey: "fixture"})
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "batch-refusal", Goal: "public evidence"})
	if err != nil {
		t.Fatal(err)
	}
	batch := []ToolCall{{ID: "first", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"first query"}`}}, {ID: "second", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"second query"}`}}}
	raw, _ := json.Marshal([]Message{{Role: "assistant", ToolCalls: batch}})
	if err = s.Data.DB.Model(&r).Update("plan_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	if err = s.Advance(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("permanent failure still dispatched the remaining batch", calls)
	}
	stored, _ := s.Data.Run(context.Background(), r.ID)
	var b BudgetSnapshot
	json.Unmarshal([]byte(stored.BudgetJSON), &b)
	if b.Spent.SearchCalls != 1 {
		t.Fatal("aborted batch charged another external attempt")
	}
	var messages []Message
	json.Unmarshal([]byte(stored.PlanJSON), &messages)
	if len(messages) != 3 || messages[2].ToolCallID != "second" {
		t.Fatal("aborted proposal lost its paired reply")
	}
}

type ordinaryFailureSearch struct{ calls int }

func (p *ordinaryFailureSearch) Search(context.Context, string, int) (SearchResponse, error) {
	p.calls++
	return SearchResponse{}, errors.New("ordinary source observation failed")
}

func TestOrdinarySourceFailureDoesNotAbortIndependentBatch(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := &ordinaryFailureSearch{}
	s.Search = p
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "ordinary-failure", Goal: "public evidence"})
	if err != nil {
		t.Fatal(err)
	}
	batch := []ToolCall{{ID: "a", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"a"}`}}, {ID: "b", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"b"}`}}}
	raw, _ := json.Marshal([]Message{{Role: "assistant", ToolCalls: batch}})
	if err = s.Data.DB.Model(&r).Update("plan_json", string(raw)).Error; err != nil {
		t.Fatal(err)
	}
	if err = s.Advance(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	stored, _ := s.Data.Run(context.Background(), r.ID)
	if p.calls != 2 || stored.IsTerminal() {
		t.Fatal("ordinary source feedback was treated as a global provider refusal")
	}
}

type deadlineSearch struct{ deadline time.Time }

func (p *deadlineSearch) Search(ctx context.Context, _ string, _ int) (SearchResponse, error) {
	p.deadline, _ = ctx.Deadline()
	return SearchResponse{Candidates: []SearchCandidate{}}, nil
}
func TestExternalToolReceivesOriginalRunDeadline(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	p := &deadlineSearch{}
	s.Search = p
	r, err := s.Create(context.Background(), CreateRunRequest{RequestKey: "tool-deadline", Goal: "public evidence"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.executeDurableTool(context.Background(), r, ToolCall{ID: "search", Type: "function", Function: FunctionCall{Name: "discover_sources", Arguments: `{"query":"public evidence"}`}}); err != nil {
		t.Fatal(err)
	}
	if p.deadline.IsZero() || p.deadline.Sub(r.Deadline) > time.Millisecond || r.Deadline.Sub(p.deadline) > time.Millisecond {
		t.Fatal("external tool can overrun run deadline")
	}
}

package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/acquisition"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/engine"
	"go-lua-crawler/internal/testdb"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHTTPSettlementKeepsUnknownOutcomeAndReleasesKnownZero(t *testing.T) {
	for _, known := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "known_zero"}[known], func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			spec := TaskSpec{Goal: "release", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "tag", Type: "string"}}, JoinKeys: []string{"tag"}}
			r, err := s.Create(ctx, CreateRunRequest{Goal: spec.Goal, RequestKey: "outcome", Spec: &spec})
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				out := RunnerResponse{Error: &RunnerError{Code: "execution_failed", Message: "fixture failure"}}
				if known {
					out.Result = &engine.ProgramResult{}
				}
				w.WriteHeader(422)
				json.NewEncoder(w).Encode(out)
			}))
			defer runner.Close()
			s.Runner, _ = NewRunnerClient(runner.URL, "fixture")
			call := ToolCall{ID: "unknown", Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: `{"kind":"adapter","source_id":"releases","purpose":"release facts","url":"https://example.com/releases","code":"return true,{}"}`}}
			_, err = s.executeDurableTool(ctx, r, call)
			if err == nil {
				t.Fatal("failure hidden")
			}
			current, _ := s.Data.Run(ctx, r.ID)
			var budget BudgetSnapshot
			json.Unmarshal([]byte(current.BudgetJSON), &budget)
			limits := programLimitsForRun(r)
			wantRequests, wantBytes := int64(limits.MaxRequests), limits.MaxBytes
			if known {
				wantRequests, wantBytes = 0, 0
			}
			if budget.Spent.HTTPRequests != wantRequests || budget.Spent.HTTPBytes != wantBytes {
				t.Fatalf("wrong settlement: %+v", budget.Spent)
			}
			ops, _ := s.Data.Operations(ctx, r.ID)
			// Duplicate completion cannot mutate either the receipt or the budget.
			err = s.Data.CompleteOperation(ctx, r.ID, r.Token, ops[0].ID, "failed", "{}", "", func(raw string, op agentdata.Operation) (string, error) {
				return settleHTTPBudget(raw, op, &HTTPObservation{Known: true})
			})
			if err == nil {
				t.Fatal("duplicate completion accepted")
			}
			again, _ := s.Data.Run(ctx, r.ID)
			if again.BudgetJSON != current.BudgetJSON {
				t.Fatal("duplicate completion refunded twice")
			}
		})
	}
}

func TestFailedTrialsRetainObservedHTTPAttempts(t *testing.T) {
	for _, mode := range []string{"runtime_failure", "wrong_fields", "healthy"} {
		t.Run(mode, func(t *testing.T) {
			s := NewService(testdb.Open(t))
			if err := s.Migrate(); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			spec := TaskSpec{Goal: "release", SchemaVersion: "v1", RequiredFields: []FieldRequirement{{Name: "tag", Type: "string"}}, JoinKeys: []string{"tag"}}
			r, err := s.Create(ctx, CreateRunRequest{Goal: spec.Goal, RequestKey: mode, Spec: &spec, SourceScope: []string{"example.com"}})
			if err != nil {
				t.Fatal(err)
			}
			r, err = s.Data.Claim(ctx, r.ID, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			runner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fields := map[string]any{"tag": "v1", "source_url": "https://example.com/releases"}
				if mode == "wrong_fields" {
					fields["tag"] = true
				}
				out := RunnerResponse{Result: &engine.ProgramResult{Collection: map[string]any{"items": []any{fields}}, Stats: acquisition.Stats{Requests: 3, Bytes: 77}, DurationMS: 12, Requests: []acquisition.Receipt{{Method: "GET", URL: "https://example.com/releases", Status: 200, FetchedAt: time.Now()}}}}
				if mode == "runtime_failure" {
					out.Error = &RunnerError{Code: "execution_failed", Message: "parse failed after IO"}
					w.WriteHeader(422)
				}
				json.NewEncoder(w).Encode(out)
			}))
			defer runner.Close()
			s.Runner, _ = NewRunnerClient(runner.URL, "test")
			call := ToolCall{ID: "trial", Type: "function", Function: FunctionCall{Name: "validate_program", Arguments: `{"kind":"adapter","source_id":"releases","purpose":"release facts","url":"https://example.com/releases","code":"return true,{}"}`}}
			out, err := s.executeDurableTool(ctx, r, call)
			if (mode == "healthy") != (err == nil) {
				t.Fatalf("wrong result %v", err)
			}
			if out.HTTP == nil || !out.HTTP.Known || out.HTTP.Requests != 3 {
				t.Fatalf("lost physical attempts after %s: %+v", mode, out)
			}
			ops, _ := s.Data.Operations(ctx, r.ID)
			var saved ToolResult
			json.Unmarshal([]byte(ops[0].ResultJSON), &saved)
			if saved.HTTP == nil || saved.HTTP.Requests != 3 {
				t.Fatalf("ledger dropped usage: %s", ops[0].ResultJSON)
			}
			current, _ := s.Data.Run(ctx, r.ID)
			var budget BudgetSnapshot
			json.Unmarshal([]byte(current.BudgetJSON), &budget)
			if budget.Spent.HTTPRequests != 3 || budget.Spent.HTTPBytes != 77 {
				t.Fatalf("observed usage not settled: %+v", budget.Spent)
			}
			// A replay after reconstructing the service must neither re-run IO nor
			// refund the same reservation again.
			restored := NewService(s.Data.DB)
			_, _ = restored.executeDurableTool(ctx, current, call)
			current, _ = s.Data.Run(ctx, r.ID)
			json.Unmarshal([]byte(current.BudgetJSON), &budget)
			if budget.Spent.HTTPRequests != 3 || budget.Spent.HTTPBytes != 77 {
				t.Fatalf("replay changed settled usage: %+v", budget.Spent)
			}
		})
	}
}

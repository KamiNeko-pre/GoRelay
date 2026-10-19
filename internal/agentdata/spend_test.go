package agentdata

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func spendRun(t *testing.T) (*Store, Run) {
	t.Helper()
	s := NewStore(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	one := int64(1000000)
	spend := SpendBudget{Policy: SpendPolicy{MaxTokens: 20, MaxMicrounits: 20, Currency: "fixture", PriceDate: "2026-10-09", Models: map[string]SpendRate{"fixture": {Input: &one, Output: &one}}, SearchPerCall: &one}}
	raw, _ := json.Marshal(map[string]any{"spend": spend, "untouched": "frozen loop facts"})
	r, err := s.CreateRun(context.Background(), "spend", "test budget", "{}", string(raw), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Claim(context.Background(), r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}

func TestSpendReservationsSurviveFailuresUnknownAndReplay(t *testing.T) {
	s, r := spendRun(t)
	ctx := context.Background()
	quote := SpendQuote{Stage: "decision", Model: "fixture", InputTokens: 6, OutputTokens: 2, Basis: "fixture upper bound"}
	first, fresh, err := s.ReserveOperationWithSpend(ctx, r.ID, r.Token, "request-1", "input-1", "model", nil, &quote)
	if err != nil || !fresh {
		t.Fatal(err)
	}
	if err = s.CompleteOperation(ctx, r.ID, r.Token, first.ID, "failed", "{}", "null"); err != nil {
		t.Fatal(err)
	}
	second, _, err := s.ReserveOperationWithSpend(ctx, r.ID, r.Token, "request-2", "input-2", "model", nil, &quote)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CompleteOperation(ctx, r.ID, r.Token, second.ID, "unknown_external", "{}", "null"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ReserveOperationWithSpend(ctx, r.ID, r.Token, "request-3", "input-3", "model", nil, &quote); !errors.Is(err, ErrSpendAdmission) {
		t.Fatalf("unknown attempt refunded: %v", err)
	}
	restored := NewStore(s.DB)
	replay, fresh, err := restored.ReserveOperationWithSpend(ctx, r.ID, r.Token, "request-1", "input-1", "model", nil, nil)
	if err != nil || fresh || replay.ID != first.ID {
		t.Fatalf("replay recharged or needed provider: %v", err)
	}
	stored, err := s.Run(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := SpendOf(stored.BudgetJSON)
	if err != nil {
		t.Fatal(err)
	}
	if budget.ReservedTokens != 16 || budget.ReservedMicrounits != 16 {
		t.Fatalf("lost reservations: %+v", budget)
	}
	var raw map[string]any
	json.Unmarshal([]byte(stored.BudgetJSON), &raw)
	if raw["untouched"] != "frozen loop facts" {
		t.Fatal("cost reservation erased loop state")
	}
	if first.CostJSON == "" || second.CostJSON == "" {
		t.Fatal("operation has no original quote")
	}
}

func TestSpendProtectsBackgroundIndexAndAllowsLocalOperations(t *testing.T) {
	s, r := spendRun(t)
	ctx := context.Background()
	if _, _, err := s.ReserveOperation(ctx, r.ID, r.Token, "local-read", "local-input", "read_observation", nil); err != nil {
		t.Fatal(err)
	}
	quote := SpendQuote{Stage: "index_embedding", Model: "fixture", InputTokens: 21, Basis: "fixture batch"}
	if _, _, err := s.ReserveIndexSpend(ctx, r.ID, "index:1", &quote); !errors.Is(err, ErrSpendAdmission) {
		t.Fatalf("index exceeded original task budget: %v", err)
	}
	quote.InputTokens = 5
	op, fresh, err := s.ReserveIndexSpend(ctx, r.ID, "index:1", &quote)
	if err != nil || !fresh || op.CostJSON == "" {
		t.Fatal("funded index rejected", err)
	}
	op2, fresh, err := s.ReserveIndexSpend(ctx, r.ID, "index:1", &quote)
	if err != nil || fresh || op2.ID != op.ID {
		t.Fatal("index replay charged twice", err)
	}
}

func TestSpendMissingRatesAndMonetaryLimitReject(t *testing.T) {
	s, r := spendRun(t)
	ctx := context.Background()
	quote := SpendQuote{Stage: "decision", Model: "unpriced", InputTokens: 1, Basis: "fixture"}
	if _, _, err := s.ReserveOperationWithSpend(ctx, r.ID, r.Token, "missing-price", "input", "model", nil, &quote); !errors.Is(err, ErrSpendAdmission) {
		t.Fatal("unknown model price admitted", err)
	}
	quote = SpendQuote{Stage: "search", Basis: "fixed search fee"}
	if _, _, err := s.ReserveOperationWithSpend(ctx, r.ID, r.Token, "expensive-search", "search", "discover_sources", nil, &quote); !errors.Is(err, ErrSpendAdmission) {
		t.Fatal("monetary limit ignored", err)
	}
}

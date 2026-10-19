package aiapp

import (
	"context"
	"encoding/json"
	"go-lua-crawler/internal/testdb"
	"testing"
)

func TestHostTaskBudgetConfigurationFreezesAndSurvivesReplay(t *testing.T) {
	t.Setenv("GORELAY_EXPERIMENT_UNLIMITED_BUDGET", "0")
	t.Setenv("GORELAY_RUN_MODEL_CALLS", "3")
	t.Setenv("GORELAY_RUN_SEARCH_CALLS", "1")
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	request := CreateRunRequest{RequestKey: "capped", Goal: "collect public evidence"}
	r, err := s.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var budget BudgetSnapshot
	if err = json.Unmarshal([]byte(r.BudgetJSON), &budget); err != nil {
		t.Fatal(err)
	}
	if budget.Limits.ModelCalls != 3 || budget.Limits.SearchCalls != 1 || budget.Unlimited || budget.PromptPolicy == nil || budget.Deadline.IsZero() {
		t.Fatalf("host time/capacity policy was not frozen: %+v", budget)
	}
	t.Setenv("GORELAY_RUN_MODEL_CALLS", "10")
	replayed, err := s.Create(context.Background(), request)
	if err != nil || replayed.BudgetJSON != r.BudgetJSON {
		t.Fatal("replay changed the frozen budget", err)
	}
}

func TestInvalidHostCapsDoNotCreateRuns(t *testing.T) {
	t.Setenv("GORELAY_EXPERIMENT_UNLIMITED_BUDGET", "0")
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"0", "-1", "not-an-integer"} {
		t.Setenv("GORELAY_RUN_SEARCH_CALLS", value)
		if _, err := s.Create(context.Background(), CreateRunRequest{RequestKey: value, Goal: "collect public evidence"}); err == nil {
			t.Fatal("invalid limit accepted", value)
		}
	}
}

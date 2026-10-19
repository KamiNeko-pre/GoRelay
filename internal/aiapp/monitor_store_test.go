package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"go-lua-crawler/internal/agentdata"
	"go-lua-crawler/internal/testdb"
	"sync"
	"testing"
	"time"
)

func TestMonitorStoreIdempotentCreatePausedAndRefresh(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := monitorSpec()
	in := CreateMonitorRequest{RequestKey: "watch", Goal: spec.Goal, Spec: &spec, Conditions: []MonitorCondition{{Field: "amount", Operator: "lte", Value: json.RawMessage(`5000`)}}}
	m, err := s.CreateMonitor(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if m.Enabled || m.ActiveRunID != "" || m.NextRunAt != nil {
		t.Fatal("creation started work or enabled schedule")
	}
	again, err := s.CreateMonitor(ctx, in)
	if err != nil || again.ID != m.ID {
		t.Fatal("duplicate creation", again, err)
	}
	in.Goal = "different"
	copy := *in.Spec
	copy.Goal = in.Goal
	in.Spec = &copy
	if _, err = s.CreateMonitor(ctx, in); !errors.Is(err, agentdata.ErrConflict) {
		t.Fatal("request key accepted different config", err)
	}
	run, err := s.RefreshMonitor(ctx, m.ID, "manual-1")
	if err != nil {
		t.Fatal(err)
	}
	againRun, err := s.RefreshMonitor(ctx, m.ID, "manual-1")
	if err != nil || againRun.ID != run.ID {
		t.Fatal("retry duplicated run", err)
	}
	if _, err = s.RefreshMonitor(ctx, m.ID, "manual-2"); !errors.Is(err, agentdata.ErrConflict) {
		t.Fatal("overlapping monitor refresh accepted", err)
	}
	var count int64
	db.Model(&MonitorCheck{}).Where("monitor_id = ?", m.ID).Count(&count)
	if count != 1 {
		t.Fatal("wrong check count", count)
	}
	current, err := s.Monitor(ctx, m.ID)
	if err != nil || current.ActiveRunID != run.ID {
		t.Fatal(current, err)
	}
	var budget BudgetSnapshot
	if err := json.Unmarshal([]byte(run.BudgetJSON), &budget); err != nil {
		t.Fatal(err)
	}
	if budget.Spent.ModelCalls != 0 {
		t.Fatal("creating refresh called a model")
	}
}

func TestMonitorConcurrentRefreshDoesNotCreateTwoRuns(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := monitorSpec()
	m, err := s.CreateMonitor(ctx, CreateMonitorRequest{RequestKey: "parallel-watch", Goal: spec.Goal, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, key := range []string{"one", "two"} {
		wg.Add(1)
		go func(key string) { defer wg.Done(); _, e := s.RefreshMonitor(ctx, m.ID, key); results <- e }(key)
	}
	wg.Wait()
	close(results)
	successes := 0
	for e := range results {
		if e == nil {
			successes++
		} else if !errors.Is(e, agentdata.ErrConflict) {
			t.Fatal(e)
		}
	}
	if successes != 1 {
		t.Fatal("expected one refresh", successes)
	}
}

func TestMonitorPauseAndConditionUpdatePreserveRunningContract(t *testing.T) {
	db := testdb.Open(t)
	s := NewService(db)
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	spec := monitorSpec()
	in := CreateMonitorRequest{RequestKey: "config", Goal: spec.Goal, Spec: &spec}
	m, err := s.CreateMonitor(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	m, err = s.UpdateMonitor(ctx, m.ID, UpdateMonitorRequest{Enabled: &on})
	if err != nil || !m.Enabled || m.NextRunAt == nil {
		t.Fatal(m, err)
	}
	run, err := s.RefreshMonitor(ctx, m.ID, "first")
	if err != nil {
		t.Fatal(err)
	}
	in.Conditions = []MonitorCondition{{Field: "amount", Operator: "lte", Value: json.RawMessage(`1000`)}}
	if _, err = s.UpdateMonitor(ctx, m.ID, UpdateMonitorRequest{Config: &in}); !errors.Is(err, agentdata.ErrConflict) {
		t.Fatal("changed config beneath active run", err)
	}
	off := false
	m, err = s.UpdateMonitor(ctx, m.ID, UpdateMonitorRequest{Enabled: &off})
	if err != nil || m.Enabled || m.NextRunAt != nil || m.ActiveRunID != run.ID {
		t.Fatal("pause cancelled or lost active run", m, err)
	}
	// Pausing future checks is distinct from cancelling the accepted Run.
	stored, _ := s.Data.Run(ctx, run.ID)
	if stored.Cancelled {
		t.Fatal("pause silently cancelled running check")
	}
	if time.Since(m.CreatedAt) > time.Minute {
		t.Fatal("unexpected timestamp")
	}
}

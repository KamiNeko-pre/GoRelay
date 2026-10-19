package aiapp

import (
	"testing"
	"time"
)

func TestRunBudgetSurvivesSnapshotAndChangingSource(t *testing.T) {
	now := time.Now().UTC()
	b, err := NewRunBudget(BudgetSnapshot{Limits: BudgetLimits{ModelCalls: 3, SearchCalls: 1, ToolCalls: 5, HTTPRequests: 4, HTTPBytes: 100, CodeCandidatesPerOperation: 2}, Deadline: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Reserve(CallCost{ModelCalls: 1, SearchCalls: 1}, now); err != nil {
		t.Fatal(err)
	}
	if err = b.ReserveCandidate("source-a/parent-1/acquire", now); err != nil {
		t.Fatal(err)
	}
	if err = b.ReserveCandidate("source-a/parent-1/acquire", now); err != nil {
		t.Fatal(err)
	}
	if err = b.ReserveCandidate("source-a/parent-1/acquire", now); err == nil {
		t.Fatal("third candidate accepted")
	}
	restored, err := NewRunBudget(b.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.ReserveCandidate("source-b/parent-1/acquire", now); err != nil {
		t.Fatal("new source incorrectly shares local candidate counter")
	}
	if err = restored.Reserve(CallCost{SearchCalls: 1}, now); err == nil {
		t.Fatal("changing source/reset restored run search budget")
	}
	if restored.Snapshot().Spent.SearchCalls != 1 {
		t.Fatal("failed reservation mutated counter")
	}
}

func TestRunBudgetDeadlineCancellationAndBytes(t *testing.T) {
	now := time.Now().UTC()
	snapshot := BudgetSnapshot{Limits: DefaultBudgetLimits(), Deadline: now.Add(time.Minute)}
	b, _ := NewRunBudget(snapshot)
	if err := b.Reserve(CallCost{HTTPBytes: snapshot.Limits.HTTPBytes + 1}, now); err == nil {
		t.Fatal("byte budget missing")
	}
	if err := b.Reserve(CallCost{ModelCalls: -1}, now); err == nil {
		t.Fatal("negative cost refunded budget")
	}
	if err := b.Reserve(CallCost{ToolCalls: 1}, now.Add(time.Minute)); err == nil {
		t.Fatal("expired run accepted")
	}
	b.Cancel()
	if err := b.Reserve(CallCost{ToolCalls: 1}, now); err == nil {
		t.Fatal("cancelled run accepted")
	}
	restored, _ := NewRunBudget(b.Snapshot())
	if err := restored.ReserveCandidate("source/parent/op", now); err == nil {
		t.Fatal("cancellation lost after restore")
	}
}

func TestExperimentalUnlimitedBudgetPreservesAccountingAndCancellation(t *testing.T) {
	now := time.Now().UTC()
	b, err := NewRunBudget(BudgetSnapshot{Limits: DefaultBudgetLimits(), Unlimited: true, Deadline: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 40; n++ {
		if err := b.Reserve(CallCost{ModelCalls: 1, SearchCalls: 1, ToolCalls: 1}, now); err != nil {
			t.Fatal(err)
		}
		if err := b.ReserveCandidate("same-host/composition", now); err != nil {
			t.Fatal(err)
		}
	}
	restored, err := NewRunBudget(b.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if restored.Snapshot().Spent.ModelCalls != 40 || restored.Snapshot().Candidates["same-host/composition"] != 40 {
		t.Fatal("accounting/reset lost")
	}
	restored.Cancel()
	if err := restored.Reserve(CallCost{ModelCalls: 1}, now); err == nil {
		t.Fatal("unlimited disabled cancellation")
	}
	if err := b.Reserve(CallCost{ModelCalls: -1}, now); err == nil {
		t.Fatal("unlimited accepted negative cost")
	}
}

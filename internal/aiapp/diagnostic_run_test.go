package aiapp

import (
	"context"
	"testing"
	"time"
)

func TestUnlimitedDiagnosticKeepsExplorationAndCancellation(t *testing.T) {
	t.Run("budget-counters-bypassed", func(t *testing.T) {
		s := decisionState()
		s.Budget.LoopVersion = 3
		s.Budget.Unlimited = true
		s.Budget.Deadline = time.Time{}
		s.Run.Deadline = time.Time{}
		s.Budget.Spent.ModelCalls = 100
		action, err := RunTaskLoop(context.Background(), s)
		if err != nil || action.Kind != "model" {
			t.Fatalf("unlimited budget counter reappeared: %+v %v", action, err)
		}
		s.Budget.Unlimited = false
		s.Budget.Deadline = time.Now().Add(time.Hour)
		action, err = RunTaskLoop(context.Background(), s)
		if err != nil || action.Kind != "finish" {
			t.Fatalf("deployment cap disappeared: %+v %v", action, err)
		}
	})
	t.Run("stagnation-recorded-not-fatal-in-diagnostic", func(t *testing.T) {
		// MVP §0.5: first-run diagnostic mode removes automatic failure on
		// stagnation/model rounds; NoProgress still accumulates for the trace
		// and deployment mode keeps the stop.
		s := decisionState()
		s.Budget.LoopVersion = 3
		s.Budget.Unlimited = true
		s.Budget.Deadline = time.Time{}
		s.Run.Deadline = time.Time{}
		s.HostProgress.NoProgressModelRounds = 100
		action, err := RunTaskLoop(context.Background(), s)
		if err != nil || action.Kind == "finish" {
			t.Fatalf("diagnostic mode failed on stagnation: %+v %v", action, err)
		}
		s.Budget.Unlimited = false
		s.Budget.Deadline = time.Now().Add(time.Hour)
		action, err = RunTaskLoop(context.Background(), s)
		if err != nil || action.Kind != "finish" || !action.HostStop {
			t.Fatalf("deployment mode lost host stagnation stop: %+v %v", action, err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		s := decisionState()
		s.Budget.Unlimited = true
		s.Budget.Cancelled = true
		if _, err := RunTaskLoop(context.Background(), s); err == nil {
			t.Fatal("diagnostic ignored cancellation")
		}
	})
}

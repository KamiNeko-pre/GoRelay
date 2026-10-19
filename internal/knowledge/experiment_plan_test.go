package knowledge

import (
	"context"
	"errors"
	"testing"
)

func TestForceRebuildBaselineStillRequiresLearnerDecision(t *testing.T) {
	pending := func(context.Context, IndexInput) (IndexPlan, error) { return IndexPlan{}, ErrCorePending }
	if _, err := ExperimentalPlanner(pending, true)(context.Background(), IndexInput{}); !errors.Is(err, ErrCorePending) {
		t.Fatal("comparison bypassed learner core")
	}
	reuse := func(context.Context, IndexInput) (IndexPlan, error) {
		return IndexPlan{Mode: "reuse", ParentGeneration: "old"}, nil
	}
	if plan, err := ExperimentalPlanner(reuse, true)(context.Background(), IndexInput{}); err != nil || plan.Mode != "rebuild" || plan.ParentGeneration != "" {
		t.Fatal("full rebuild baseline reused vectors")
	}
	if plan, _ := ExperimentalPlanner(reuse, false)(context.Background(), IndexInput{}); plan.Mode != "reuse" {
		t.Fatal("normal indexing lost reuse")
	}
}

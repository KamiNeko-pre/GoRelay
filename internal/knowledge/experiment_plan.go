package knowledge

import "context"

// Force rebuilding is a controlled comparison, not a default implementation of
// the learner's eligibility decision. Rejected/unfinished decisions stay so.
func ExperimentalPlanner(decide func(context.Context, IndexInput) (IndexPlan, error), force bool) func(context.Context, IndexInput) (IndexPlan, error) {
	return func(ctx context.Context, in IndexInput) (IndexPlan, error) {
		plan, err := decide(ctx, in)
		if err != nil {
			return plan, err
		}
		if force && plan.Mode == "reuse" {
			plan.Mode = "rebuild"
			plan.ParentGeneration = ""
		}
		return plan, nil
	}
}

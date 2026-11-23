package information

import "context"

// lifecycleAfterInvocation maps a runner outcome to scheduling state. The
// presentation stage never chooses a model action.
func lifecycleAfterInvocation(stage string, finished bool, wait *informationWait) string {
	if finished {
		return "completed"
	}
	if stage == "needs_input" {
		return "needs_input"
	}
	if terminal(stage) {
		return stage
	}
	if wait != nil {
		if len(wait.Dependencies) > 0 {
			return "waiting_dependency"
		}
		if wait.WakeAt != nil || wait.Stage != "" {
			return "running"
		}
	}
	if stage == "blocked_runtime" {
		return "failed"
	}
	if stage == "running" || stage == "queued" || stage == "waiting" {
		return "running"
	}
	return "needs_input"
}

func terminalStages() []string {
	return []string{"completed", "partial", "failed", "cancelled", "expired"}
}
func terminal(stage string) bool {
	for _, value := range terminalStages() {
		if stage == value {
			return true
		}
	}
	return false
}

// Advance delegates model/tool sequencing exclusively to Eino ADK. Stage is a
// durable user-visible projection; it does not select a model action.
func (s *Service) Advance(ctx context.Context, id string) error {
	request, err := s.Request(ctx, id)
	if err != nil {
		return err
	}
	if request.Purpose == nativeSourcePurpose {
		return s.advanceNativeSourceJob(ctx, request)
	}
	return s.advanceEino(ctx, id)
}

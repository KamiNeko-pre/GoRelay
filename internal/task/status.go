package task

type Status string

const (
	StatusPending   Status = "pending"
	StatusQueued    Status = "queued"
	StatusRunning   Status = "running"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

func CanTransition(from, to Status) bool {
	switch from {
	case StatusPending:
		return to == StatusQueued || to == StatusFailed || to == StatusCancelled
	case StatusQueued:
		return to == StatusRunning || to == StatusFailed || to == StatusCancelled
	case StatusRunning:
		return to == StatusSucceeded || to == StatusFailed || to == StatusCancelled
	default:
		return false
	}
}
func (status Status) IsTerminal() bool {
	return status == StatusSucceeded || status == StatusFailed || status == StatusCancelled
}

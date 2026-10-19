package agentstatus

import (
	"fmt"
	"math"
	"time"
)

// Nil selects the deployment default; explicit zero means no task timebox.
// Callers choose budgets for actual difficulty, never model call IDs or rounds.
func RequestTimeout(seconds *int64, fallback time.Duration) (time.Duration, error) {
	if seconds == nil {
		return fallback, nil
	}
	if *seconds < 0 || *seconds > math.MaxInt64/int64(time.Second) {
		return 0, fmt.Errorf("timeout_seconds must be nonnegative and representable as a duration")
	}
	return time.Duration(*seconds) * time.Second, nil
}

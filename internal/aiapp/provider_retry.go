package aiapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-lua-crawler/internal/agentdata"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func providerRetryTime(err error, attempt int, now time.Time) time.Time {
	delay := time.Second*time.Duration(1<<uint(attempt)) + time.Duration(rand.IntN(251))*time.Millisecond
	var remote *ProviderError
	if errors.As(err, &remote) {
		raw := strings.TrimSpace(remote.RetryAfter)
		if seconds, e := strconv.ParseInt(raw, 10, 32); e == nil && seconds >= 0 {
			delay = max(delay, time.Duration(seconds)*time.Second)
		} else if date, e := http.ParseTime(raw); e == nil {
			delay = max(delay, date.Sub(now))
		}
	}
	return now.Add(delay)
}

// Persist timing with the already-charged budget; restarts cannot bypass it.
func (s *Service) scheduleProviderRetry(ctx context.Context, r agentdata.Run, cause error, attempt int, round *LoopRound, originalTimes ...time.Time) error {
	if attempt >= 2 {
		return s.stopUnavailableProvider(ctx, r, fmt.Errorf("model failed after three attempts: %w", cause), round, "")
	}
	when := providerRetryTime(cause, attempt, time.Now())
	if len(originalTimes) > 0 {
		when = originalTimes[0]
	}
	if !r.Deadline.IsZero() && !when.Before(r.Deadline) {
		return s.stopUnavailableProvider(ctx, r, fmt.Errorf("provider retry cannot fit remaining task deadline: %w", cause), round, "")
	}
	current, err := s.Data.Run(ctx, r.ID)
	if err != nil {
		return err
	}
	var budget BudgetSnapshot
	if err = json.Unmarshal([]byte(current.BudgetJSON), &budget); err != nil {
		return err
	}
	budget.RetryNotBefore = &when
	raw, err := json.Marshal(budget)
	if err != nil {
		return err
	}
	return s.saveLoopRun(ctx, r, map[string]any{"status": "waiting", "next_wake_at": when, "budget_json": string(raw), "last_error": cause.Error()}, round, cause)
}

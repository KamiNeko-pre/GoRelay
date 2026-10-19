package aiapp

import (
	"context"
	"go-lua-crawler/internal/testdb"
	"testing"
	"time"
)

func TestProviderBackoffPersistsSchedulerWakeTime(t *testing.T) {
	s := NewService(testdb.Open(t))
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := s.Create(ctx, CreateRunRequest{Goal: "facts", RequestKey: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	r, err = s.Data.Claim(ctx, r.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(time.Minute)
	round := &LoopRound{RoundID: "retry", StartedAt: time.Now()}
	if err = s.scheduleProviderRetry(ctx, r, &ProviderError{Status: 503, Retryable: true}, 0, round, when); err != nil {
		t.Fatal(err)
	}
	current, err := s.Data.Run(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.NextWakeAt == nil || current.NextWakeAt.Sub(when) > time.Millisecond || when.Sub(*current.NextWakeAt) > time.Millisecond {
		t.Fatalf("provider backoff not scheduled: %+v", current.NextWakeAt)
	}
	pending, err := s.Data.Pending(ctx, 8)
	if err != nil || len(pending) != 0 {
		t.Fatal("retry was polled before its due time", err)
	}
}

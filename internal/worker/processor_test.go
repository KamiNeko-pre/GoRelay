package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/redis/go-redis/v9"
	"gorelay/internal/fetchgov"
	"gorelay/internal/pipeline"
	"gorelay/internal/programrunner"
	taskmodel "gorelay/internal/task"
)

func TestNewProcessorDependencies(t *testing.T) {
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	t.Cleanup(func() { _ = client.Close() })
	cfg := DefaultConfig("test-stream", "test-group", "test-consumer")
	if _, err := NewProcessor(nil, cfg); err == nil {
		t.Fatal("nil client must be rejected")
	}
	if _, err := NewProcessor(client, Config{}); err == nil {
		t.Fatal("invalid config must be rejected")
	}
	processor, err := NewProcessor(client, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if processor.client != client || processor.config != cfg {
		t.Fatal("processor did not retain its Redis dependencies")
	}
}

func TestClassifyExecutionErrorPreservesDriftGovernanceAndTimeoutCodes(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		err        error
		cancelled  bool
		wantStatus taskmodel.Status
		wantCode   string
	}{
		{
			name:       "silent drift preserved",
			err:        fmt.Errorf("%w: DRIFT_ZERO_ROWS", pipeline.ErrSilentDrift),
			wantStatus: taskmodel.StatusFailed,
			wantCode:   "silent_drift",
		},
		{
			name:       "governance error mapped to lowercase code",
			err:        &fetchgov.GovernanceError{Code: fetchgov.ErrCodeRateLimited, Message: "HTTP 429"},
			wantStatus: taskmodel.StatusFailed,
			wantCode:   "rate_limited",
		},
		{
			name:       "runner error code preserved across container boundary",
			err:        &programrunner.Error{Code: "CIRCUIT_OPEN", Message: "domain in cooldown"},
			wantStatus: taskmodel.StatusFailed,
			wantCode:   "circuit_open",
		},
		{
			name:       "deadline exceeded maps to timeout",
			err:        fmt.Errorf("run failed: %w", context.DeadlineExceeded),
			wantStatus: taskmodel.StatusFailed,
			wantCode:   "timeout",
		},
		{
			name:       "explicit cancel marks status cancelled",
			err:        context.Canceled,
			cancelled:  true,
			wantStatus: taskmodel.StatusCancelled,
			wantCode:   "cancelled",
		},
		{
			name:       "generic failure falls back to execution_error",
			err:        errors.New("unexpected upstream JSON"),
			wantStatus: taskmodel.StatusFailed,
			wantCode:   "execution_error",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, code, msg := classifyExecutionError(ctx, 42, tc.err, func(context.Context, uint64) bool { return tc.cancelled })
			if st != tc.wantStatus || code != tc.wantCode || msg == "" {
				t.Fatalf("got (%s, %q, %q), want (%s, %q, non-empty)", st, code, msg, tc.wantStatus, tc.wantCode)
			}
		})
	}
}

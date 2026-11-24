package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorelay/internal/config"
	"go.uber.org/zap"
)

func TestPublisherLoopRejectsInvalidSettings(t *testing.T) {
	for _, cfg := range []config.PublisherConfig{
		{BatchSize: 0, PollIntervalMS: 1, BatchTimeoutSeconds: 1},
		{BatchSize: 1, PollIntervalMS: 0, BatchTimeoutSeconds: 1},
		{BatchSize: 1, PollIntervalMS: 1, BatchTimeoutSeconds: 0},
	} {
		err := runPublisherLoop(context.Background(), cfg, func(context.Context, int) (int, error) {
			t.Fatal("invalid settings must not publish")
			return 0, nil
		}, zap.NewNop())
		if err == nil {
			t.Fatal("expected configuration error")
		}
	}
}

func TestPublisherLoopRetriesAndStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cfg := config.PublisherConfig{BatchSize: 7, PollIntervalMS: 1, BatchTimeoutSeconds: 1}
	calls := 0
	err := runPublisherLoop(ctx, cfg, func(batchCtx context.Context, limit int) (int, error) {
		calls++
		if limit != 7 {
			t.Errorf("limit = %d, want 7", limit)
		}
		if _, ok := batchCtx.Deadline(); !ok {
			t.Error("batch must have a deadline")
		}
		if calls == 1 {
			return 2, errors.New("temporary Redis failure")
		}
		cancel()
		return 1, nil
	}, zap.NewNop())
	if err != nil || calls != 2 {
		t.Fatalf("error=%v calls=%d, want clean shutdown after two calls", err, calls)
	}
}

func TestPublisherLoopAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runPublisherLoop(ctx, config.PublisherConfig{BatchSize: 1, PollIntervalMS: 1, BatchTimeoutSeconds: 1},
		func(context.Context, int) (int, error) {
			t.Fatal("cancelled loop must not publish")
			return 0, nil
		}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
}

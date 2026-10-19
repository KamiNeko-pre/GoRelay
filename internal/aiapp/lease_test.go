package aiapp

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestLeaseHeartbeatKeepsSlowWorkAliveAndCancelsLostOwnership(t *testing.T) {
	for _, key := range []string{"search", "Lua validation"} {
		t.Run(key, func(t *testing.T) {
			var calls atomic.Int64
			ctx, stop := keepLease(context.Background(), time.Millisecond, func(context.Context) error { calls.Add(1); return nil })
			time.Sleep(15 * time.Millisecond)
			if ctx.Err() != nil || calls.Load() == 0 {
				t.Fatal("slow work lost ownership")
			}
			stop()
			if ctx.Err() == nil {
				t.Fatal("stop did not cancel heartbeat")
			}
		})
	}
	ctx, stop := keepLease(context.Background(), time.Millisecond, func(context.Context) error { return errors.New("ownership lost") })
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("external work continued after ownership loss")
	}
}

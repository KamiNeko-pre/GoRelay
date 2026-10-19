package aiapp

import (
	"context"
	"time"
)

// Long model/tool batches retain ownership without holding a SQL transaction.
// Losing the lease cancels external work; the replacement owner resumes from
// durable receipts, and unknown external attempts remain explicitly unknown.
func keepLease(parent context.Context, interval time.Duration, renew func(context.Context) error) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := renew(ctx); err != nil {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

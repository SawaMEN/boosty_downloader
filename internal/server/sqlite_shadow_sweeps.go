package server

import (
	"context"
	"time"
)

// Wake is a one-element notification queue, not a count of changes.
// Producers persist their changes before sending a wake. All notifications
// already queued before sweep begins are covered by its fresh snapshot.
// Never drain AFTER the sweep: an update during reconciliation may need
// another pass. Periodic sweeps remain mandatory, even without a wake.
func runSQLiteShadowSweeps(ctx context.Context, wake <-chan struct{}, ticks <-chan time.Time, delay time.Duration, sweep func(context.Context)) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		case <-ticks:
		}
		// Both production channels have at most one pending event.
		// Bounded drains avoid starvation under continuous writes.
		select {
		case <-wake:
		default:
		}
		select {
		case <-ticks:
		default:
		}
		if ctx.Err() != nil {
			return
		}
		sweep(ctx)
	}
}

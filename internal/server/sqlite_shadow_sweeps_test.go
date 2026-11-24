package server

import (
	"context"
	"testing"
	"time"
)

func TestSQLiteShadowSweepsCoalesceAndPreserveConcurrentWake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	wake := make(chan struct{}, 1)
	ticks := make(chan time.Time, 1)
	wake <- struct{}{}
	ticks <- time.Now()
	calls := 0
	runSQLiteShadowSweeps(ctx, wake, ticks, 0, func(context.Context) {
		calls++
		if len(wake) != 0 || len(ticks) != 0 {
			t.Fatal("queued signals were not coalesced before sweep")
		}
		if calls == 1 {
			// An update during the first sweep must schedule another one.
			wake <- struct{}{}
		} else {
			cancel()
		}
	})
	if calls != 2 {
		t.Fatalf("got %d sweeps, want 2", calls)
	}
}

func TestSQLiteShadowSweepsPeriodicWithoutWake(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	wake := make(chan struct{}, 1)
	ticks := make(chan time.Time, 1)
	ticks <- time.Now()
	calls := 0
	runSQLiteShadowSweeps(ctx, wake, ticks, 0, func(context.Context) {
		calls++
		cancel()
	})
	if calls != 1 {
		t.Fatalf("periodic full audit skipped: %d", calls)
	}
}

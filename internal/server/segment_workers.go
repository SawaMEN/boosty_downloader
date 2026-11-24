package server

import (
	"context"
	"os"
)

type segmentWorkersKey struct{}

func validSegmentWorkers(n int) bool {
	return n >= 0 && n <= 3
}

// Snapshot the administrator's choice for the lifetime of one archive job.
// Zero retains the per-protocol deployment environment for compatibility.
func withSegmentWorkers(ctx context.Context, n *int) context.Context {
	if n == nil {
		return ctx
	}
	return context.WithValue(ctx, segmentWorkersKey{}, *n)
}

func segmentWorkers(ctx context.Context, env string) int {
	n, _ := ctx.Value(segmentWorkersKey{}).(int)
	if n == 3 {
		return adaptiveSegmentWorkers()
	}
	if n == 1 || n == 2 {
		return n
	}
	if os.Getenv(env) == "2" {
		return 2
	}
	return 1
}

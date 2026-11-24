package server

import (
	"context"
	"testing"
	"time"
)

func TestSegmentWorkersPolicyPrecedence(t *testing.T) {
	t.Setenv("BD_HLS_SEGMENT_WORKERS", "2")
	t.Setenv("BD_DASH_SEGMENT_WORKERS", "")
	for _, n := range []int{0, 1, 2} {
		ctx := withSegmentWorkers(context.Background(), &n)
		hls, dash := n, n
		if n == 0 {
			hls, dash = 2, 1
		}
		if segmentWorkers(ctx, "BD_HLS_SEGMENT_WORKERS") != hls || segmentWorkers(ctx, "BD_DASH_SEGMENT_WORKERS") != dash {
			t.Fatalf("wrong precedence for %d", n)
		}
	}
}

func TestSegmentWorkersPolicyPersistence(t *testing.T) {
	root := t.TempDir()
	a, err := newAccounts(root, "admin", "strong-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.initializePolicy(time.Hour); err != nil {
		t.Fatal(err)
	}
	p := a.policy()
	if p.SegmentWorkers == nil || *p.SegmentWorkers != 0 {
		t.Fatal("wrong legacy default")
	}
	n := 2
	p.SegmentWorkers = &n
	if err := a.changePolicy(p); err != nil {
		t.Fatal(err)
	}
	n = 1
	if *a.policy().SegmentWorkers != 2 {
		t.Fatal("policy retained caller pointer")
	}
	p.SegmentWorkers = nil
	if err := a.changePolicy(p); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []int{-1, 4} {
		p.SegmentWorkers = &invalid
		if err := a.changePolicy(p); err == nil {
			t.Fatal("invalid workers accepted")
		}
	}
	again, err := newAccounts(root, "admin", "strong-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := again.initializePolicy(time.Hour); err != nil {
		t.Fatal(err)
	}
	if p := again.policy(); p.SegmentWorkers == nil || *p.SegmentWorkers != 2 {
		t.Fatal("workers lost on reload")
	}
}

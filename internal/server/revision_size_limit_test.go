package server

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRevisionSizedPrefix(t *testing.T) {
	sizes := []int64{8, 5, 1, 2}
	for _, tc := range []struct {
		limit int
		bytes int64
		want  int
	}{
		{4, 0, 4},
		{2, 0, 2},
		{4, 7, 1},
		{4, 8, 1},
		{4, 12, 1},
		{4, 13, 2},
		{4, 14, 3},
		{2, 100, 2},
	} {
		got, err := revisionSizedPrefix(context.Background(), len(sizes), tc.limit, tc.bytes, func(i int) (int64, error) {
			return sizes[i], nil
		})
		if err != nil || got != tc.want {
			t.Fatalf("limit=%d bytes=%d: got %d want %d, %v", tc.limit, tc.bytes, got, tc.want, err)
		}
	}
}

func TestRevisionSizedPrefixFailsClosed(t *testing.T) {
	failure := errors.New("failed scan")
	if _, err := revisionSizedPrefix(context.Background(), 3, 3, 10, func(int) (int64, error) {
		return 0, failure
	}); !errors.Is(err, failure) {
		t.Fatalf("lost scan error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := revisionSizedPrefix(ctx, 3, 3, 10, func(int) (int64, error) {
		t.Fatal("canceled scan continued")
		return 0, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}

func TestRevisionMaxGiBPolicyPersistence(t *testing.T) {
	root := t.TempDir()
	a, err := newAccounts(root, "admin", "strong-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.initializePolicy(24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	p := a.policy()
	if p.RevisionMaxGiB == nil || p.revisionMaxGiB() != 0 {
		t.Fatal("new limit not disabled by default")
	}
	value := 12
	p.RevisionMaxGiB = &value
	if err := a.changePolicy(p); err != nil {
		t.Fatal(err)
	}
	p.RevisionMaxGiB = nil
	if err := a.changePolicy(p); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []int{-1, 16385} {
		p.RevisionMaxGiB = &invalid
		if err := a.changePolicy(p); err == nil {
			t.Fatal("invalid size limit accepted")
		}
	}
	again, err := newAccounts(root, "admin", "strong-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := again.initializePolicy(24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	if again.policy().revisionMaxGiB() != 12 {
		t.Fatal("size limit not retained")
	}
	p = again.policy()
	zero := 0
	p.RevisionMaxGiB = &zero
	if err := again.changePolicy(p); err != nil || again.policy().revisionMaxGiB() != 0 {
		t.Fatalf("cannot disable size limit: %v", err)
	}
}

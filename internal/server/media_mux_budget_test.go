package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMediaMuxOutputLimit(t *testing.T) {
	root := t.TempDir()
	ctx := withStorageBudget(context.Background(), root, "owner", 0, 1)
	b := ctx.Value(storageBudgetKey{}).(*storageBudget)
	b.initialized, b.used, b.maxUser = true, 7, 10
	limit, err := mediaMuxOutputLimit(ctx, 100)
	if err != nil || limit != 3 {
		t.Fatalf("limit=%d err=%v", limit, err)
	}
	b.used = 10
	if _, err := mediaMuxOutputLimit(ctx, 100); err == nil {
		t.Fatal("zero remaining quota allowed mux")
	}
}

func TestBoundedMediaMuxRejectsFastOverflow(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mp4")
	err := runBoundedMediaMux(context.Background(), "/bin/sh", []string{"-c", "printf 123456789 > \"$1\"", "fixture", output}, output, 5)
	if !errors.Is(err, errMediaMuxLimit) {
		t.Fatalf("fast overflow accepted: %v", err)
	}
}

func TestBoundedMediaMuxStopsRunningOverflow(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runBoundedMediaMux(ctx, "/bin/sh", []string{"-c", "printf 123456789 > \"$1\"; while :; do :; done", "fixture", output}, output, 5)
	if !errors.Is(err, errMediaMuxLimit) {
		t.Fatalf("running overflow not stopped: %v", err)
	}
}

func TestBoundedMediaMuxAcceptsSmallOutputAndRejectsExisting(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mp4")
	args := []string{"-c", "printf 123 > \"$1\"", "fixture", output}
	if err := runBoundedMediaMux(context.Background(), "/bin/sh", args, output, 5); err != nil {
		t.Fatal(err)
	}
	if err := runBoundedMediaMux(context.Background(), "/bin/sh", args, output, 5); err == nil {
		t.Fatal("existing output was reused")
	}
	got, err := os.ReadFile(output)
	if err != nil || string(got) != "123" {
		t.Fatalf("existing output modified: %q %v", got, err)
	}
}

func TestBoundedMediaMuxCancellation(t *testing.T) {
	output := filepath.Join(t.TempDir(), "out.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	err := runBoundedMediaMux(ctx, "/bin/sh", []string{"-c", "while :; do :; done"}, output, 5)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

// The longer rendition runner shares the same logical quota and output
// enforcement even when the encoder writes more than one polling interval
// would normally observe.
func TestBoundedRenditionRejectsQuotaOverflow(t *testing.T) {
	root := t.TempDir()
	ctx := withStorageBudget(context.Background(), root, "owner", 0, 1)
	b := ctx.Value(storageBudgetKey{}).(*storageBudget)
	b.initialized, b.used, b.maxUser = true, 7, 10
	output := filepath.Join(root, "rendition.mp4")
	err := runBoundedMediaMuxFor(ctx, "/bin/sh", []string{"-c", "printf 1234 > \"$1\"", "fixture", output}, output, maxArchiveMediaSize, time.Second)
	if !errors.Is(err, errMediaMuxLimit) {
		t.Fatalf("rendition exceeded remaining quota: %v", err)
	}
	if b.used != 7 {
		t.Fatal("failed rendition charged logical quota")
	}
}

func TestBoundedRenditionHonorsTimeout(t *testing.T) {
	output := filepath.Join(t.TempDir(), "rendition.mp4")
	err := runBoundedMediaMuxFor(context.Background(), "/bin/sh",
		[]string{"-c", "while :; do :; done"}, output, maxArchiveMediaSize, 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rendition timeout lost: %v", err)
	}
}

package server

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type heldMediaDiskWriter struct {
	entered chan struct{}
	release chan struct{}
}

func (w heldMediaDiskWriter) Write(p []byte) (int, error) {
	close(w.entered)
	<-w.release
	return len(p), nil
}

func TestMediaDiskGateAcrossBudgets(t *testing.T) {
	root := t.TempDir()
	first := withStorageBudget(context.Background(), root, "alice", 0, 1)
	second := withStorageBudget(context.Background(), root, "bob", 0, 1)
	writer := heldMediaDiskWriter{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := guardedCopy(first, writer, strings.NewReader("a"))
		done <- err
	}()
	defer func() {
		close(writer.release)
		if err := <-done; err != nil {
			t.Errorf("first write failed: %v", err)
		}
	}()
	select {
	case <-writer.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first write did not enter")
	}
	for _, base := range []context.Context{second, context.Background()} {
		ctx, cancel := context.WithTimeout(base, 50*time.Millisecond)
		var out bytes.Buffer
		n, err := guardedCopy(ctx, &out, strings.NewReader("b"))
		cancel()
		if n != 0 || out.Len() != 0 || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("concurrent write bypassed gate: n=%d out=%d err=%v", n, out.Len(), err)
		}
	}
}

func TestMediaDiskGateReleaseAndCancellation(t *testing.T) {
	release, err := acquireMediaDiskWrite(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if unlock, err := acquireMediaDiskWrite(ctx); !errors.Is(err, context.Canceled) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("canceled acquisition: %v", err)
	}
	release()
	release()
	nextCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	var out bytes.Buffer
	if n, err := guardedCopy(nextCtx, &out, strings.NewReader("ok")); n != 2 || err != nil || out.String() != "ok" {
		t.Fatalf("gate did not reopen: %d %v", n, err)
	}
}

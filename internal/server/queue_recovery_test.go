package server

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestPendingQueueRecoveryLargeBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &App{ctx: ctx}
	jobs := make(chan string, 128)
	imports := make(chan string, 64)
	ids := make([]string, 1000)
	for i := range ids {
		ids[i] = fmt.Sprintf("job-%d", i)
	}
	a.restorePendingQueue(jobs, ids)
	a.restorePendingQueue(imports, []string{"import-one"})
	// Imports must remain independent while no archive consumer runs.
	select {
	case got := <-imports:
		if got != "import-one" {
			t.Fatal(got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("import recovery blocked behind archive backlog")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for _, want := range ids {
		select {
		case got := <-jobs:
			if got != want {
				t.Fatalf("lost or reordered ID: got %s want %s", got, want)
			}
		case <-deadline.C:
			t.Fatal("archive backlog did not drain")
		}
	}
	a.wg.Wait()
	if len(jobs) != 0 {
		t.Fatal("unexpected duplicate delivery")
	}
}

func TestPendingQueueRecoveryCancelsWhenFull(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &App{ctx: ctx}
	queue := make(chan string, 1)
	queue <- "already-full"
	a.restorePendingQueue(queue, []string{"pending-one", "pending-two"})
	cancel()
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queue recovery prevented shutdown")
	}
	if len(queue) != 1 || <-queue != "already-full" {
		t.Fatal("shutdown modified full queue")
	}
}

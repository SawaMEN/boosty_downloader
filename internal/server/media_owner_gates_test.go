package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMediaOwnerGateCancellationAndIsolation(t *testing.T) {
	var gates mediaOwnerGates
	first, err := gates.acquire(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if unlock, err := gates.acquire(ctx, "alice"); !errors.Is(err, context.DeadlineExceeded) {
		if unlock != nil {
			unlock()
		}
		t.Fatalf("same owner did not wait: %v", err)
	}
	other := ""
	for i := 0; i < 1000; i++ {
		name := fmt.Sprintf("owner-%d", i)
		if gates.gate(name) != gates.gate("alice") {
			other = name
			break
		}
	}
	if other == "" {
		t.Fatal("no independent shard")
	}
	otherCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	unlock, err := gates.acquire(otherCtx, other)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	first()
	first()
	next, err := gates.acquire(otherCtx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	next()
}

func TestConcurrentGuardedWritesRespectSharedQuota(t *testing.T) {
	ctx := withStorageBudget(context.Background(), t.TempDir(), "owner", 0, 1)
	b := ctx.Value(storageBudgetKey{}).(*storageBudget)
	b.initialized, b.maxUser = true, 7
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan int, 32)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var out bytes.Buffer
			_, err := guardedCopy(ctx, &out, strings.NewReader("x"))
			if err == nil {
				results <- out.Len()
			} else {
				results <- -out.Len()
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	written := 0
	for n := range results {
		if n < 0 {
			t.Fatal("failed quota write reached disk")
		}
		written += n
	}
	if written != 7 || b.used != 7 {
		t.Fatalf("concurrent writes exceeded or lost quota: %d/%d", written, b.used)
	}
}

func TestVideoRenditionBudgetRejectsLargerOutput(t *testing.T) {
	ctx := withStorageBudget(context.Background(), t.TempDir(), "owner", 0, 1)
	b := ctx.Value(storageBudgetKey{}).(*storageBudget)
	b.initialized, b.used, b.maxUser = true, 7, 10
	if err := checkVideoRenditionBudget(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if err := checkVideoRenditionBudget(ctx, 4); err == nil {
		t.Fatal("over-quota rendition accepted")
	}
	if b.used != 7 {
		t.Fatal("validation modified accounting")
	}
}

package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestDASHParallelTrackOrder(t *testing.T) {
	t.Setenv("BD_DASH_SEGMENT_WORKERS", "2")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dir := t.TempDir()
	started := make(chan string, 2)
	both := make(chan struct{})
	var count atomic.Int32
	fetch := func(ctx context.Context, raw, _, _ string) (*http.Response, error) {
		started <- raw
		if count.Add(1) == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return fakeMedia(200, raw, nil, int64(len(raw))), nil
	}
	budget := 2 * maxHLSSegmentBytes
	path, err := fetchDASHTrack(ctx, fetch, dashTrack{init: "init", segments: []string{"part"}}, dir, "track.mp4", &budget)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "initpart" || budget != 2*maxHLSSegmentBytes-8 || len(started) != 2 {
		t.Fatalf("wrong track or budget: %q %d %v", data, budget, err)
	}
}

func TestDASHParallelBatchCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dir := t.TempDir()
	started, stopped := make(chan struct{}), make(chan struct{})
	fetch := func(ctx context.Context, raw, _, _ string) (*http.Response, error) {
		if raw == "bad" {
			select {
			case <-started:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return fakeMedia(403, "", nil, 0), nil
		}
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	tasks := []dashSegmentTask{{url: "bad", path: filepath.Join(dir, "a")}, {url: "wait", path: filepath.Join(dir, "b")}}
	if _, err := fetchDASHSegmentBatch(ctx, fetch, tasks, 2*maxHLSSegmentBytes); !mediaExpired(err) {
		t.Fatalf("lost original CDN error: %v", err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("worker not joined")
	}
	if _, err := fetchDASHSegmentBatch(ctx, fetch, tasks, 2*maxHLSSegmentBytes-1); err == nil {
		t.Fatal("over-budget pair accepted")
	}
}

func TestDASHParallelFallsBackNearBudget(t *testing.T) {
	t.Setenv("BD_DASH_SEGMENT_WORKERS", "2")
	dir := t.TempDir()
	budget := int64(8)
	calls := 0
	fetch := func(_ context.Context, raw, _, _ string) (*http.Response, error) {
		calls++
		if (calls == 1 && raw != "init") || (calls == 2 && raw != "part") {
			return nil, errors.New("unexpected fetch order")
		}
		return fakeMedia(200, raw, nil, int64(len(raw))), nil
	}
	if _, err := fetchDASHTrack(context.Background(), fetch, dashTrack{init: "init", segments: []string{"part"}}, dir, "track.mp4", &budget); err != nil || calls != 2 || budget != 0 {
		t.Fatalf("sequential fallback: %d %d %v", calls, budget, err)
	}
}

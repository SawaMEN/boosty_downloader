package server

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
)

func TestAdaptiveSegmentThresholds(t *testing.T) {
	for _, tc := range []struct {
		cpu, load float64
		memory    uint64
		want      int
	}{
		{1, 0, 1 << 30, 1},
		{2, 1.5, 1 << 30, 1},
		{2, 1, adaptiveMemoryReserve - 1, 1},
		{2, 1, adaptiveMemoryReserve, 2},
		{4, 2, 1 << 30, 2},
	} {
		if got := chooseAdaptiveSegmentWorkers(tc.cpu, tc.load, tc.memory); got != tc.want {
			t.Fatalf("resources %+v: got %d", tc, got)
		}
	}
}

func TestMediaSegmentBatchBusyGateFallsBack(t *testing.T) {
	mediaExtraSegmentGate <- struct{}{}
	defer func() { <-mediaExtraSegmentGate }()
	dir := t.TempDir()
	calls := 0
	fetch := func(_ context.Context, raw, _, _ string) (*http.Response, error) {
		calls++
		if (calls == 1 && raw != "first") || (calls == 2 && raw != "second") {
			t.Fatalf("unexpected ordered request %s", raw)
		}
		return fakeMedia(200, raw, nil, int64(len(raw))), nil
	}
	tasks := []mediaSegmentTask{{url: "first", path: filepath.Join(dir, "a")}, {url: "second", path: filepath.Join(dir, "b")}}
	sizes, err := fetchMediaSegmentBatch(context.Background(), fetch, tasks, 2*maxHLSSegmentBytes)
	if err != nil || calls != 2 || sizes != [2]int64{5, 6} || len(mediaExtraSegmentGate) != 1 {
		t.Fatalf("fallback failed: %v %v %d", sizes, err, calls)
	}
}

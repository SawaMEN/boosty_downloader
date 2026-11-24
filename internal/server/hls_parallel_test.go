package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHLSParallelTrackPreservesRangesAndPlaylist(t *testing.T) {
	t.Setenv("BD_HLS_SEGMENT_WORKERS", "2")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	dir := t.TempDir()
	both := make(chan struct{})
	var calls atomic.Int32
	fetch := func(ctx context.Context, raw, byteRange, _ string) (*http.Response, error) {
		n := calls.Add(1)
		if n == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		switch byteRange {
		case "bytes=0-1":
			return fakeMedia(206, "ab", map[string]string{"Content-Range": "bytes 0-1/6"}, 2), nil
		case "bytes=2-3":
			return fakeMedia(206, "cd", map[string]string{"Content-Range": "bytes 2-3/6"}, 2), nil
		case "bytes=4-5":
			return fakeMedia(206, "ef", map[string]string{"Content-Range": "bytes 4-5/6"}, 2), nil
		default:
			return nil, fmt.Errorf("unexpected range: %s", byteRange)
		}
	}
	raw := "https://cdn.example/track.mp4"
	parts := []hlsPart{
		{url: raw, initURL: raw, initLength: 2, byteOffset: 2, byteLength: 2, duration: 1},
		{url: raw, initURL: raw, initLength: 2, byteOffset: 4, byteLength: 2, duration: 2, discontinuity: true},
	}
	var total int64
	local, err := stageHLSTrack(ctx, fetch, dir, "video", parts, &total)
	if err != nil || total != 6 || calls.Load() != 3 {
		t.Fatalf("staging failed: total=%d calls=%d err=%v", total, calls.Load(), err)
	}
	for name, want := range map[string]string{"video-init-000000.mp4": "ab", "video-segment-000000.mp4": "cd", "video-segment-000001.mp4": "ef"} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", name, got, err)
		}
	}
	data, err := os.ReadFile(local)
	want := "#EXT-X-MAP:URI=\"video-init-000000.mp4\"\n#EXTINF:1.000000,\nvideo-segment-000000.mp4\n#EXT-X-DISCONTINUITY\n#EXTINF:2.000000,\nvideo-segment-000001.mp4\n#EXT-X-ENDLIST\n"
	if err != nil || !strings.HasSuffix(string(data), want) {
		t.Fatalf("playlist order: %s %v", data, err)
	}
}

func TestHLSParallelNearLimitUsesSequentialFetch(t *testing.T) {
	t.Setenv("BD_HLS_SEGMENT_WORKERS", "2")
	dir := t.TempDir()
	total := maxArchiveMediaSize - 4
	calls := 0
	fetch := func(_ context.Context, raw, _, _ string) (*http.Response, error) {
		calls++
		if (calls == 1 && raw != "first.ts") || (calls == 2 && raw != "second.ts") {
			return nil, fmt.Errorf("wrong request order")
		}
		return fakeMedia(200, "ab", nil, 2), nil
	}
	parts := []hlsPart{{url: "first.ts", duration: 1}, {url: "second.ts", duration: 1}}
	if _, err := stageHLSTrack(context.Background(), fetch, dir, "audio", parts, &total); err != nil || total != maxArchiveMediaSize || calls != 2 {
		t.Fatalf("sequential fallback: total=%d calls=%d err=%v", total, calls, err)
	}
}

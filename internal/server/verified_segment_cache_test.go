package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifiedSegmentCacheRevalidation(t *testing.T) {
	for _, mode := range []string{"same", "changed-url", "changed-etag", "weak-etag", "missing-etag", "ignored-range", "changed-size", "damaged-local"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cleanup := withVerifiedSegmentCache(context.Background(), filepath.Join(dir, "movie.mp4"))
			defer cleanup()
			raw := "https://cdn.example/segment?token=first"
			calls, probes := 0, 0
			fetch := func(_ context.Context, _ string, byteRange, ifRange string) (*http.Response, error) {
				calls++
				if byteRange == "" {
					return fakeMedia(200, "abcdef", map[string]string{"ETag": "\"v1\""}, 6), nil
				}
				probes++
				if byteRange != "bytes=0-0" || ifRange != "\"v1\"" {
					t.Fatal("invalid conditional probe")
				}
				headers := map[string]string{"ETag": "\"v1\"", "Content-Range": "bytes 0-0/6"}
				switch mode {
				case "changed-etag":
					headers["ETag"] = "\"v2\""
				case "weak-etag":
					headers["ETag"] = "W/\"v1\""
				case "missing-etag":
					delete(headers, "ETag")
				case "changed-size":
					headers["Content-Range"] = "bytes 0-0/7"
				case "ignored-range":
					return fakeMedia(200, "abcdef", headers, 6), nil
				}
				return fakeMedia(206, "a", headers, 1), nil
			}
			first := filepath.Join(dir, "first")
			if _, err := fetchHLSSegment(ctx, fetch, raw, first, 100); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(first); err != nil {
				t.Fatal(err)
			}
			if mode == "damaged-local" {
				cache := ctx.Value(verifiedSegmentCacheKey{}).(*verifiedSegmentCache)
				if err := os.WriteFile(filepath.Join(cache.dir, cache.entries[raw].name), []byte("abcdeg"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "changed-url" {
				raw = "https://cdn.example/segment?token=second"
			}
			second := filepath.Join(dir, "second")
			n, err := fetchHLSSegment(ctx, fetch, raw, second, 100)
			if err != nil || n != 6 {
				t.Fatalf("second segment failed: %d %v", n, err)
			}
			data, err := os.ReadFile(second)
			if err != nil || string(data) != "abcdef" {
				t.Fatal("cache returned invalid bytes")
			}
			wantCalls, wantProbes := 3, 1
			if mode == "same" {
				wantCalls = 2
			}
			if mode == "changed-url" {
				wantCalls, wantProbes = 2, 0
			}
			if calls != wantCalls || probes != wantProbes {
				t.Fatalf("calls=%d probes=%d want=%d/%d", calls, probes, wantCalls, wantProbes)
			}
			cache := ctx.Value(verifiedSegmentCacheKey{}).(*verifiedSegmentCache)
			cleanup()
			if _, err := os.Stat(cache.dir); !os.IsNotExist(err) {
				t.Fatal("cache was not removed")
			}
		})
	}
}

package server

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifiedRangeCacheSeparatesOffsetsAndValidators(t *testing.T) {
	for _, mode := range []string{"same", "offset", "length", "total", "validator", "redirect", "byte", "encoding"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cleanup := withVerifiedSegmentCache(context.Background(), filepath.Join(dir, "video.mp4"))
			defer cleanup()
			const raw = "https://cdn.example/movie.mp4"
			calls, probes := 0, 0
			fetch := func(_ context.Context, _ string, byteRange, ifRange string) (*http.Response, error) {
				calls++
				headers := map[string]string{"ETag": "\"v1\""}
				if ifRange != "" {
					probes++
					if byteRange != "bytes=2-2" || ifRange != "\"v1\"" {
						t.Fatalf("invalid probe: %q %q", byteRange, ifRange)
					}
					headers["Content-Range"] = "bytes 2-2/10"
					data := "c"
					switch mode {
					case "total":
						headers["Content-Range"] = "bytes 2-2/11"
					case "validator":
						headers["ETag"] = "\"v2\""
					case "byte":
						data = "x"
					case "encoding":
						headers["Content-Encoding"] = "gzip"
					}
					res := fakeMedia(206, data, headers, 1)
					if mode == "redirect" {
						res.Request = &http.Request{URL: &url.URL{Scheme: "https", Host: "other.example", Path: "/movie.mp4"}}
					}
					return res, nil
				}
				switch byteRange {
				case "bytes=2-4":
					headers["Content-Range"] = "bytes 2-4/10"
					return fakeMedia(206, "cde", headers, 3), nil
				case "bytes=5-7":
					headers["Content-Range"] = "bytes 5-7/10"
					return fakeMedia(206, "fgh", headers, 3), nil
				case "bytes=2-5":
					headers["Content-Range"] = "bytes 2-5/10"
					return fakeMedia(206, "cdef", headers, 4), nil
				}
				t.Fatalf("unexpected range %s", byteRange)
				return nil, nil
			}
			if _, err := fetchHLSRangeSegment(ctx, fetch, raw, filepath.Join(dir, "first"), 100, 2, 3); err != nil {
				t.Fatal(err)
			}
			offset, length, want := int64(2), int64(3), "cde"
			if mode == "offset" {
				offset, want = 5, "fgh"
			}
			if mode == "length" {
				length, want = 4, "cdef"
			}
			second := filepath.Join(dir, "second")
			n, err := fetchHLSRangeSegment(ctx, fetch, raw, second, 100, offset, length)
			if err != nil || n != length {
				t.Fatalf("range recovery: %d %v", n, err)
			}
			got, err := os.ReadFile(second)
			if err != nil || string(got) != want {
				t.Fatalf("range bytes: %q %v", got, err)
			}
			wantCalls, wantProbes := 3, 1
			if mode == "same" {
				wantCalls = 2
			}
			if mode == "offset" || mode == "length" {
				wantCalls, wantProbes = 2, 0
			}
			if calls != wantCalls || probes != wantProbes {
				t.Fatalf("requests=%d probes=%d want=%d/%d", calls, probes, wantCalls, wantProbes)
			}
		})
	}
}

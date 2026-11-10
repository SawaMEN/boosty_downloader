package server

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHLSByteRangeSpec(t *testing.T) {
	cases := []struct {
		spec           string
		prev           int64
		allowed        bool
		offset, length int64
		valid          bool
	}{
		{"4@0", 0, false, 0, 4, true},
		{"4", 4, true, 4, 4, true},
		{"4", 4, false, 0, 0, false},
		{"0@0", 0, false, 0, 0, false},
		{"500000000000@0", 0, false, 0, 0, false},
		{"4@-1", 0, false, 0, 0, false},
		{"4@999999999999", 0, false, 0, 0, false},
	}
	for _, tc := range cases {
		start, n, e := parseHLSRangeSpec(tc.spec, tc.prev, tc.allowed)
		if (e == nil) != tc.valid || e == nil && (start != tc.offset || n != tc.length) {
			t.Errorf("%q %d %d %v", tc.spec, start, n, e)
		}
	}
}
func TestHLSByteRangeFetch(t *testing.T) {
	for _, status := range []int{206, 200} {
		dir := t.TempDir()
		path := filepath.Join(dir, "part.ts")
		fetch := func(_ context.Context, _, requested, _ string) (*http.Response, error) {
			if requested != "bytes=4-7" {
				t.Fatalf("range header %q", requested)
			}
			h := make(http.Header)
			h.Set("Content-Range", "bytes 4-7/8")
			return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader("efgh")), ContentLength: 4}, nil
		}
		n, e := fetchHLSRangeSegment(context.Background(), fetch, "https://cdn.example/file", path, 10, 4, 4)
		if status == 206 {
			if e != nil || n != 4 {
				t.Fatalf("valid range %v %d", e, n)
			}
		} else {
			if e == nil {
				t.Fatal("ignored range accepted")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("invalid range written")
			}
		}
	}
}
func TestHLSByteRangeRejectWrongRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.ts")
	fetch := func(_ context.Context, _, _, _ string) (*http.Response, error) {
		h := make(http.Header)
		h.Set("Content-Range", "bytes 5-8/12")
		return &http.Response{StatusCode: 206, Header: h, Body: io.NopCloser(strings.NewReader("efgh")), ContentLength: 4}, nil
	}
	if _, e := fetchHLSRangeSegment(context.Background(), fetch, "https://cdn.example/file", path, 10, 4, 4); e == nil {
		t.Fatal("mismatched range accepted")
	}
}

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeMedia(status int, data string, headers map[string]string, length int64) *http.Response {
	h := make(http.Header)
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(data)), ContentLength: length}
}
func shaText(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func TestDownloadInterruptedResumesWithETag(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "movie.mp4")
	calls := 0
	fetch := func(_ context.Context, _, rangeHeader, ifRange string) (*http.Response, error) {
		calls++
		if calls == 1 {
			if rangeHeader != "" || ifRange != "" {
				t.Fatal("first call should not resume")
			}
			return fakeMedia(200, "hello", map[string]string{"ETag": "\"revision-1\""}, 10), nil
		}
		if rangeHeader != "bytes=5-" || ifRange != "\"revision-1\"" {
			t.Fatalf("resume headers: %q %q", rangeHeader, ifRange)
		}
		return fakeMedia(206, "world", map[string]string{"ETag": "\"revision-1\"", "Content-Range": "bytes 5-9/10"}, 5), nil
	}
	if _, _, err := downloadFile(context.Background(), fetch, "https://cdn.example/video", dest, 10); err == nil {
		t.Fatal("truncated response must fail")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("truncated file visible as complete")
	}
	n, digest, err := downloadFile(context.Background(), fetch, "https://cdn.example/video", dest, 10)
	if err != nil || n != 10 || digest != shaText("helloworld") {
		t.Fatalf("resume result %d %s %v", n, digest, err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "helloworld" {
		t.Fatalf("final bytes %s", b)
	}
	if _, err := os.Stat(dest + ".part.meta"); !os.IsNotExist(err) {
		t.Fatal("metadata not removed")
	}
}

func TestDownloadIfRangeReplacedRestartsWithoutMixing(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "movie.mp4")
	calls := 0
	fetch := func(_ context.Context, _, byteRange, ifRange string) (*http.Response, error) {
		calls++
		if calls == 1 {
			return fakeMedia(200, "OLD", map[string]string{"ETag": "\"old\""}, 8), nil
		}
		if byteRange != "bytes=3-" || ifRange != "\"old\"" {
			t.Fatalf("unexpected resume %q %q", byteRange, ifRange)
		}
		return fakeMedia(200, "NEW_DATA", map[string]string{"ETag": "\"new\""}, 8), nil
	}
	if _, _, err := downloadFile(context.Background(), fetch, "https://cdn.example/video", dest, 0); err == nil {
		t.Fatal("truncated response must fail")
	}
	size, digest, err := downloadFile(context.Background(), fetch, "https://cdn.example/video", dest, 0)
	if err != nil || size != 8 || digest != shaText("NEW_DATA") {
		t.Fatalf("new representation not respected: %v", err)
	}
}

func TestDownloadWithoutValidatorRestarts(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "movie.mp4")
	os.WriteFile(dest+".part", []byte("OLD"), 0600)
	fetch := func(_ context.Context, _, byteRange, ifRange string) (*http.Response, error) {
		if byteRange != "" || ifRange != "" {
			t.Fatalf("unverifiable partial should be discarded: %q", byteRange)
		}
		return fakeMedia(200, "NEW", nil, 3), nil
	}
	size, _, err := downloadFile(context.Background(), fetch, "https://cdn.example/video", dest, 0)
	if err != nil || size != 3 {
		t.Fatalf("fresh download: %v", err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "NEW" {
		t.Fatalf("mixed bytes: %q", b)
	}
}

func TestDownloadRejectsBadContentRange(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "movie.mp4")
	var calls int
	fetch := func(_ context.Context, _, _, _ string) (*http.Response, error) {
		calls++
		if calls == 1 {
			return fakeMedia(200, "OLD", map[string]string{"ETag": "\"same\""}, 9), nil
		}
		return fakeMedia(206, "123456", map[string]string{"ETag": "\"same\"", "Content-Range": "bytes 4-9/10"}, 6), nil
	}
	_, _, _ = downloadFile(context.Background(), fetch, "https://cdn.example/video", dest, 0)
	if _, _, err := downloadFile(context.Background(), fetch, "https://cdn.example/video", dest, 0); err == nil {
		t.Fatal("wrong Content-Range accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("partial exposed as final")
	}
}

func TestDownloadRejectsPlaylistAndOversize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "movie.mp4")
	for _, ct := range []string{"application/vnd.apple.mpegurl", "application/dash+xml"} {
		_, _, err := downloadFile(context.Background(), func(context.Context, string, string, string) (*http.Response, error) {
			return fakeMedia(200, "#EXTM3U", map[string]string{"Content-Type": ct}, 7), nil
		}, "https://cdn.example/video", path, 0)
		if err == nil {
			t.Fatalf("playlist %q saved as mp4", ct)
		}
	}
	_, _, err := downloadFile(context.Background(), func(context.Context, string, string, string) (*http.Response, error) {
		return nil, fmt.Errorf("should not fetch")
	}, "https://cdn.example/video", path, maxArchiveMediaSize+1)
	if err == nil || !strings.Contains(err.Error(), "10 ГиБ") {
		t.Fatal("oversize allowed")
	}
}

func TestVerifyArchiveChecksSHA256AndMissing(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "media")
	if err := os.Mkdir(media, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(media, "a.mp4")
	if err := os.WriteFile(path, []byte("video"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := ArchiveManifest{Assets: []ArchiveAsset{{Filename: "a.mp4", Size: 5, SHA256: shaText("video")}, {Filename: "b.mp4", Size: 2, SHA256: shaText("ok")}}}
	if err := writeJSON(filepath.Join(dir, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	result, err := verifyArchive(dir)
	if err != nil || result.Valid || result.Verified != 1 || result.Missing != 1 {
		t.Fatalf("incorrect verification %+v %v", result, err)
	}
	if err := os.WriteFile(path, []byte("virus"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = verifyArchive(dir)
	if err != nil || result.Damaged != 1 {
		t.Fatalf("corruption undetected %+v %v", result, err)
	}
	if err := os.WriteFile(path, []byte("video"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(media, "b.mp4"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = verifyArchive(dir)
	if err != nil || !result.Valid || result.Verified != 2 {
		t.Fatalf("valid archive rejected %+v %v", result, err)
	}
}

func TestDownloadRejectsUnexpectedHTMLAndPlaylistURL(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "video.mp4")
	fetch := func(context.Context, string, string, string) (*http.Response, error) {
		return fakeMedia(200, "<html>login</html>", map[string]string{"Content-Type": "text/html"}, 18), nil
	}
	if _, _, err := downloadFile(context.Background(), fetch, "https://cdn.example/media.mp4", dest, 0); err == nil {
		t.Fatal("HTML response incorrectly archived as MP4")
	}
	if _, _, err := downloadFile(context.Background(), fetch, "https://cdn.example/master.m3u8?signature=private", dest, 0); err == nil {
		t.Fatal("HLS URL incorrectly archived as MP4")
	}
}

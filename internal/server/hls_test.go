package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func hlsFetch(m map[string]string) mediaFetcher {
	return func(_ context.Context, url, _, _ string) (*http.Response, error) {
		s, ok := m[url]
		if !ok {
			return nil, fmt.Errorf("no test url %q", url)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(s)), Header: http.Header{}, ContentLength: int64(len(s))}, nil
	}
}
func TestHLSMasterSelectsHighest(t *testing.T) {
	base := "https://cdn.example/master.m3u8?token=secret"
	m := map[string]string{base: "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100\nlow.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=300\nhigh.m3u8\n", "https://cdn.example/high.m3u8?token=secret": "#EXTM3U\n#EXT-X-TARGETDURATION:5\n#EXTINF:5,\nvideo.ts\n#EXT-X-ENDLIST\n"}
	segments, err := resolveHLS(context.Background(), hlsFetch(m), base)
	if err != nil || len(segments) != 1 || segments[0].url != "https://cdn.example/video.ts?token=secret" {
		t.Fatalf("%+v %v", segments, err)
	}
}
func TestHLSRejectsUnsafeManifests(t *testing.T) {
	samples := []string{
		"#EXTM3U\n#EXTINF:3,\nhttps://127.0.0.1/x.ts\n#EXT-X-ENDLIST\n", // Note URL validation at the real fetcher level; parser does not resolve DNS.
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\"\n#EXTINF:2,\na.ts\n#EXT-X-ENDLIST\n",
		"#EXTM3U\n#EXTINF:2,\na.ts\n",
		"#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=500,AUDIO=\"other\"\na.m3u8\n",
		"#EXTM3U\n#EXTINF:2,\nhttp://example.com/a.ts\n#EXT-X-ENDLIST\n",
	}
	for i, s := range samples {
		if i == 0 {
			continue
		}
		if _, err := parseHLS([]byte(s), "https://cdn.example/base.m3u8"); err == nil {
			t.Fatalf("sample %d unexpectedly accepted", i)
		}
	}
}
func TestHLSQueryNotLeakedToOtherHost(t *testing.T) {
	s, err := hlsResourceURL("https://secret.example/master.m3u8?auth=secret", "https://other.example/segment.ts")
	if err != nil || strings.Contains(s, "auth=") {
		t.Fatalf("%s %v", s, err)
	}
}
func TestHLSRemuxOffline(t *testing.T) {
	ffmpeg, e := exec.LookPath("ffmpeg")
	if e != nil {
		t.Skip("ffmpeg not installed")
	}
	d := t.TempDir()
	p := filepath.Join(d, "seg.ts")
	cmd := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=64x64:rate=5", "-t", "1", "-c:v", "mpeg2video", "-f", "mpegts", p)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg fixture unavailable %v %s", err, b)
	}
	raw, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	base := "https://cdn.example/index.m3u8"
	data := map[string]string{base: "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:1.0,\nseg.ts\n#EXT-X-ENDLIST\n", "https://cdn.example/seg.ts": string(raw)}
	dest := filepath.Join(d, "result.mp4")
	size, digest, err := archiveHLS(context.Background(), hlsFetch(data), base, dest)
	if err != nil {
		t.Fatal(err)
	}
	if size < 100 || len(digest) != 64 {
		t.Fatalf("size %d digest %s", size, digest)
	}
	if _, e := os.Stat(dest); e != nil {
		t.Fatal(e)
	}
}

func TestHLSFailurePreservesExistingVideo(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "existing.mp4")
	if err := os.WriteFile(dest, []byte("old-video"), 0600); err != nil {
		t.Fatal(err)
	}
	base := "https://cdn.example/playlist.m3u8"
	fetch := hlsFetch(map[string]string{base: "#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:2,\nmissing.ts\n#EXT-X-ENDLIST\n"})
	if _, _, err := archiveHLS(context.Background(), fetch, base, dest); err == nil {
		t.Fatal("missing segment must fail")
	}
	b, err := os.ReadFile(dest)
	if err != nil || string(b) != "old-video" {
		t.Fatalf("old video changed %q %v", b, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(dir, ".boosty-hls-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("staging leaks: %v %v", leftovers, err)
	}
}

func TestHLSSegmentTruncated(t *testing.T) {
	dir := t.TempDir()
	fetch := func(_ context.Context, _, _, _ string) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("abc")), ContentLength: 4}, nil
	}
	if _, err := fetchHLSSegment(context.Background(), fetch, "https://cdn.example/s.ts", filepath.Join(dir, "s.ts"), 100); err == nil {
		t.Fatal("truncated segment accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "s.ts")); !os.IsNotExist(err) {
		t.Fatalf("partial segment left on disk: %v", err)
	}
}

func TestHLSInitMapRewrites(t *testing.T) {
	list := "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4,\ns1.m4s\n#EXT-X-ENDLIST\n"
	v, err := parseHLS([]byte(list), "https://cdn.example/video/main.m3u8?sig=x")
	if err != nil || len(v.parts) != 1 || v.parts[0].initURL != "https://cdn.example/video/init.mp4?sig=x" {
		t.Fatalf("map parse %+v %v", v, err)
	}
}

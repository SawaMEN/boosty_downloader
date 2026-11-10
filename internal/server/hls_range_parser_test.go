package server

import (
	"bytes"
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

func TestHLSParserByteRangeAndInit(t *testing.T) {
	const base = "https://cdn.example/video.m3u8?token=signed"
	raw := `#EXTM3U
#EXT-X-MAP:URI="init.mp4",BYTERANGE="5@0"
#EXTINF:1,
#EXT-X-BYTERANGE:10@5
track.m4s
#EXTINF:1,
#EXT-X-BYTERANGE:8
track.m4s
#EXT-X-ENDLIST
`
	pl, err := parseHLS([]byte(raw), base)
	if err != nil {
		t.Fatal(err)
	}
	if len(pl.parts) != 2 {
		t.Fatalf("parts %+v", pl.parts)
	}
	if pl.parts[0].byteOffset != 5 || pl.parts[0].byteLength != 10 || pl.parts[1].byteOffset != 15 || pl.parts[1].byteLength != 8 {
		t.Fatalf("ranges %+v", pl.parts)
	}
	if pl.parts[0].initOffset != 0 || pl.parts[0].initLength != 5 || pl.parts[0].initURL != "https://cdn.example/init.mp4?token=signed" {
		t.Fatalf("init %+v", pl.parts[0])
	}
}
func TestHLSParserRejectImplicitRangeAfterNewURL(t *testing.T) {
	raw := `#EXTM3U
#EXTINF:1,
#EXT-X-BYTERANGE:4@0
a.ts
#EXTINF:1,
#EXT-X-BYTERANGE:4
b.ts
#EXT-X-ENDLIST
`
	if _, err := parseHLS([]byte(raw), "https://cdn.example/pl.m3u8"); err == nil {
		t.Fatal("implicit offset accepted for changed resource")
	}
}
func TestHLSParserRejectMalformedRanges(t *testing.T) {
	for _, spec := range []string{"0@0", "2@-1", "5@999999999999999", "5"} {
		raw := fmt.Sprintf("#EXTM3U\n#EXTINF:1,\n#EXT-X-BYTERANGE:%s\na.ts\n#EXT-X-ENDLIST\n", spec)
		if _, err := parseHLS([]byte(raw), "https://cdn.example/pl.m3u8"); err == nil {
			t.Fatalf("accepted %q", spec)
		}
	}
}
func TestHLSRangeFullArchiveRemux(t *testing.T) {
	ffmpeg, e := exec.LookPath("ffmpeg")
	if e != nil {
		t.Skip("ffmpeg missing")
	}
	dir := t.TempDir()
	ts := filepath.Join(dir, "original.ts")
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=64x64:rate=5", "-t", "2", "-c:v", "mpeg2video", "-f", "mpegts", ts}
	if b, e := exec.Command(ffmpeg, args...).CombinedOutput(); e != nil {
		t.Skipf("ffmpeg fixture %v %s", e, b)
	}
	original, e := os.ReadFile(ts)
	if e != nil {
		t.Fatal(e)
	}
	half := len(original) / 188 / 2 * 188
	if half < 188 || len(original)-half < 188 {
		t.Skip("fixture too short")
	}
	base := "https://cdn.example/video/stream?auth=secret"
	path := "https://cdn.example/video/entire.ts?auth=secret"
	manifest := fmt.Sprintf("#EXTM3U\n#EXT-X-TARGETDURATION:2\n#EXTINF:1,\n#EXT-X-BYTERANGE:%d@0\nentire.ts\n#EXTINF:1,\n#EXT-X-BYTERANGE:%d\nentire.ts\n#EXT-X-ENDLIST\n", half, len(original)-half)
	fetch := func(_ context.Context, link, rangeHeader, _ string) (*http.Response, error) {
		var data []byte
		h := make(http.Header)
		status := 200
		switch link {
		case base:
			data = []byte(manifest)
		case path:
			var start, end int
			if _, e := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); e != nil || start < 0 || end >= len(original) || start > end {
				return nil, fmt.Errorf("unexpected range %q: %v", rangeHeader, e)
			}
			data = original[start : end+1]
			status = 206
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(original)))
		default:
			return nil, fmt.Errorf("unexpected URL %q", link)
		}
		return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: int64(len(data))}, nil
	}
	dest := filepath.Join(dir, "out.mp4")
	n, digest, err := archiveHLS(context.Background(), fetch, base, dest)
	if err != nil {
		t.Fatal(err)
	}
	if n < 100 || len(digest) != 64 {
		t.Fatalf("output %d %s", n, digest)
	}
	if ffprobe, e := exec.LookPath("ffprobe"); e == nil {
		b, e := exec.Command(ffprobe, "-v", "error", "-show_entries", "stream=codec_type", "-of", "csv=p=0", dest).Output()
		if e != nil || !strings.Contains(string(b), "video") {
			t.Fatalf("ffprobe %s %v", b, e)
		}
	}
}

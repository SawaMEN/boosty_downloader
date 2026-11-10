package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestExtensionlessVideoPlaylistProbe(t *testing.T) {
	samples := []struct{ name, ctype, body, want string }{
		{"hls MIME", "application/vnd.apple.mpegurl; charset=utf-8", "anything", "hls"},
		{"dash MIME", "application/dash+xml", "anything", "dash"},
		{"hls body", "application/octet-stream", "#EXTM3U\n#EXTINF:2,\na.ts", "hls"},
		{"dash XML", "application/xml", "<?xml version=\"1.0\"?><MPD type=\"static\"/>", "dash"},
		{"direct MP4", "video/mp4", "not-a-manifest", ""},
		{"login HTML", "text/html", "<html>login</html>", ""},
	}
	for _, tc := range samples {
		t.Run(tc.name, func(t *testing.T) {
			fetch := func(_ context.Context, _, byteRange, _ string) (*http.Response, error) {
				if byteRange != "bytes=0-4095" {
					t.Fatalf("missing bounded probe: %q", byteRange)
				}
				h := make(http.Header)
				h.Set("Content-Type", tc.ctype)
				return &http.Response{StatusCode: 206, Header: h, Body: io.NopCloser(strings.NewReader(tc.body)), ContentLength: int64(len(tc.body))}, nil
			}
			if got := detectVideoPlaylist(context.Background(), fetch, "https://cdn.example/video?id=12"); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

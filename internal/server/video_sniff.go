package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
)

// detectVideoPlaylist recognizes a video manifest on CDNs that use extensionless
// signed URLs. A capped range probe avoids downloading a whole video into RAM.
// The actual download still uses the mediaFetcher's SSRF and redirect checks.
func detectVideoPlaylist(ctx context.Context, fetch mediaFetcher, raw string) string {
	res, err := fetch(ctx, raw, "bytes=0-4095", "")
	if err != nil {
		return ""
	} // Let the regular downloader report transient errors.
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return ""
	}
	mime := strings.ToLower(strings.TrimSpace(strings.Split(res.Header.Get("Content-Type"), ";")[0]))
	if strings.Contains(mime, "mpegurl") {
		return "hls"
	}
	if mime == "application/dash+xml" {
		return "dash"
	}
	if mime == "text/html" || mime == "application/json" {
		return ""
	}
	first, err := io.ReadAll(io.LimitReader(res.Body, 4096))
	if err != nil {
		return ""
	}
	b := bytes.TrimSpace(bytes.TrimPrefix(first, []byte{0xef, 0xbb, 0xbf}))
	if bytes.HasPrefix(b, []byte("#EXTM3U")) {
		return "hls"
	}
	if bytes.HasPrefix(b, []byte("<MPD")) || bytes.HasPrefix(b, []byte("<?xml")) && bytes.Contains(b, []byte("<MPD")) {
		return "dash"
	}
	return ""
}

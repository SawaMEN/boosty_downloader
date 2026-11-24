package server

import (
	"mime"
	"net/http"
	"strings"
)

// Use a narrow set of passive media MIME types. In particular, never
// render SVG, HTML, XML, JavaScript or arbitrary text in the app's origin.
// A claimed Boosty media kind must also match the advertised MIME family.
func safeInlineMediaType(raw, kind string) string {
	ct, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return ""
	}
	var family string
	switch ct {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/avif", "image/bmp", "image/x-icon", "image/vnd.microsoft.icon":
		family = "image"
	case "video/mp4", "video/webm", "video/quicktime", "video/x-m4v", "video/ogg", "video/mp2t",
		"application/vnd.apple.mpegurl", "application/x-mpegurl", "application/dash+xml":
		family = "ok_video"
	case "audio/mpeg", "audio/mp4", "audio/aac", "audio/ogg", "audio/wav", "audio/x-wav", "audio/webm", "audio/flac":
		family = "audio_file"
	default:
		return ""
	}
	if kind != "" && kind != family {
		return ""
	}
	return ct
}

// Shared by live CDN proxy and archived attachments, including revisions.
// The generated archive index has its own policy and is not an attachment.
func setIsolatedMediaHeaders(h http.Header, filename, rawType, kind string, inline bool) {
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "private, no-store")
	h.Set("Content-Type", "application/octet-stream")
	disposition := "attachment"
	if inline {
		if ct := safeInlineMediaType(rawType, kind); ct != "" {
			h.Set("Content-Type", ct)
			disposition = "inline"
		}
	}
	value := mime.FormatMediaType(disposition, map[string]string{"filename": filename})
	if strings.TrimSpace(value) == "" {
		value = "attachment"
	}
	h.Set("Content-Disposition", value)
}

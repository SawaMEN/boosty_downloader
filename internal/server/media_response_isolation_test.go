package server

import (
	"crypto/sha256"
	"fmt"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsolatedMediaHeaders(t *testing.T) {
	for _, tc := range []struct {
		raw, kind, want string
		inline          bool
	}{
		{"video/mp4", "ok_video", "video/mp4", true},
		{"IMAGE/PNG; charset=utf-8", "image", "image/png", true},
		{"audio/mpeg", "audio_file", "audio/mpeg", true},
		{"application/vnd.apple.mpegurl", "ok_video", "application/vnd.apple.mpegurl", true},
		{"text/html", "image", "", true},
		{"image/svg+xml", "image", "", true},
		{"application/xhtml+xml", "ok_video", "", true},
		{"application/javascript", "image", "", true},
		{"image/png", "file", "", true},
		{"image/png", "ok_video", "", true},
		{"image/png", "image", "", false},
		{"image/png; malformed", "image", "", true},
		{"", "", "", true},
	} {
		t.Run(tc.raw+"/"+tc.kind+fmt.Sprint(tc.inline), func(t *testing.T) {
			h := make(http.Header)
			h.Set("Content-Range", "bytes 1-2/3")
			setIsolatedMediaHeaders(h, "test.bin", tc.raw, tc.kind, tc.inline)
			disp, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
			wantType, wantDisp := tc.want, "inline"
			if wantType == "" {
				wantType, wantDisp = "application/octet-stream", "attachment"
			}
			if err != nil || params["filename"] != "test.bin" || disp != wantDisp ||
				h.Get("Content-Type") != wantType {
				t.Fatalf("wrong media headers: %v", h)
			}
			if h.Get("X-Content-Type-Options") != "nosniff" ||
				!strings.HasPrefix(h.Get("Content-Security-Policy"), "sandbox;") ||
				h.Get("Cross-Origin-Resource-Policy") != "same-origin" ||
				h.Get("Cache-Control") != "private, no-store" ||
				h.Get("Content-Range") != "bytes 1-2/3" {
				t.Fatalf("missing isolation or lost range: %v", h)
			}
		})
	}
}

func TestArchiveMediaHTTPIsolation(t *testing.T) {
	app, cookie, clip, zipPath, man := hashedZIPFixture(t, []byte("existing"))
	base := filepath.Dir(filepath.Dir(clip))
	id := strings.TrimSuffix(strings.TrimPrefix(zipPath, "/api/jobs/"), "/zip")
	for _, name := range []string{"payload.html", "payload.svg", "clip.mp4"} {
		data := []byte("<script>alert(document.domain)</script>")
		if name == "clip.mp4" {
			data = []byte("0123456789")
		}
		if err := os.WriteFile(filepath.Join(base, "media", name), data, 0600); err != nil {
			t.Fatal(err)
		}
		man.Assets = append(man.Assets, ArchiveAsset{Filename: name, Type: "file", Size: int64(len(data)), SHA256: fmt.Sprintf("%x", sha256.Sum256(data))})
	}
	if err := writeJSON(filepath.Join(base, "manifest.json"), man); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"payload.html", "payload.svg", "clip.mp4"} {
		req := reqAs("GET", "/library/"+id+"/media/"+name, "", cookie)
		if name == "clip.mp4" {
			req.Header.Set("Range", "bytes=2-5")
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		h := rec.Header()
		if name == "clip.mp4" {
			if rec.Code != 206 || rec.Body.String() != "2345" || h.Get("Content-Type") != "video/mp4" ||
				!strings.HasPrefix(h.Get("Content-Disposition"), "inline") {
				t.Fatalf("safe range broken: %d %v", rec.Code, h)
			}
		} else if rec.Code != 200 || h.Get("Content-Type") != "application/octet-stream" ||
			!strings.HasPrefix(h.Get("Content-Disposition"), "attachment") {
			t.Fatalf("unsafe media rendered inline: %d %v", rec.Code, h)
		}
		if !strings.HasPrefix(h.Get("Content-Security-Policy"), "sandbox;") {
			t.Fatal("media sandbox missing")
		}
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, reqAs("GET", "/library/"+id+"/", "", cookie))
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") ||
		rec.Header().Get("Content-Disposition") != "" {
		t.Fatalf("generated archive index no longer renders: %d %v", rec.Code, rec.Header())
	}
}

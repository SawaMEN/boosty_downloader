package server

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A ZIP is made only from files named in the manifest. Unrelated local
// artifacts must not deny service or leak into the download, while a missing
// or replaced declared file must fail before any ZIP headers are emitted.
func TestZIPManifestAllowlistPreflightAndRecovery(t *testing.T) {
	app, err := New(Config{
		AdminUser: "admin", AdminPassword: "strong-password-123",
		SecretKey: testKey(t), DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	owner := testLogin(t, app, "admin", "strong-password-123")
	j := Job{
		ID: strings.Repeat("b", 24), OwnerID: "admin", Author: "creator",
		PostID: "post-test", Title: "Saved post", State: "done",
		CreatedAt: time.Now().UTC(),
	}
	if err := app.store.upsert(j); err != nil {
		t.Fatal(err)
	}
	base := app.store.archiveDir(j.OwnerID, j.Author, j.PostID)
	mediaDir := filepath.Join(base, "media")
	if err := os.MkdirAll(mediaDir, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := ArchiveManifest{
		Title: j.Title, Author: j.Author, PostID: j.PostID,
		CapturedAt: time.Now().UTC(),
		Assets:     []ArchiveAsset{{Filename: "clip.txt", Type: "file", Size: 5}},
	}
	if err := writeJSON(filepath.Join(base, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "index.html"), []byte("<p>Saved post</p>"), 0600); err != nil {
		t.Fatal(err)
	}
	clip := filepath.Join(mediaDir, "clip.txt")
	if err := os.WriteFile(clip, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(app.store.root, "private-secret")
	if err := os.WriteFile(secret, []byte("SECRET_NOT_IN_ZIP"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(mediaDir, "unlisted-secret")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(mediaDir, "unlisted-pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "post.json"), []byte("PRIVATE_RAW_RESPONSE"), 0600); err != nil {
		t.Fatal(err)
	}

	path := "/api/jobs/" + j.ID + "/zip"
	verifyZIP := func() {
		t.Helper()
		raw := testArchiveResponse(t, app, owner, "GET", path, "", http.StatusOK)
		zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Fatalf("invalid ZIP returned: %v", err)
		}
		found := map[string]bool{}
		for _, z := range zr.File {
			found[z.Name] = true
			f, err := z.Open()
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(f)
			_ = f.Close()
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(body, []byte("PRIVATE_RAW_RESPONSE")) || bytes.Contains(body, []byte("SECRET_NOT_IN_ZIP")) {
				t.Fatal("ZIP leaked a private unlisted file")
			}
			if z.Name == "media/clip.txt" && string(body) != "hello" {
				t.Fatalf("wrong archived bytes: %q", body)
			}
		}
		for _, name := range []string{"index.html", "manifest.json", "media/clip.txt"} {
			if !found[name] {
				t.Fatalf("missing declared ZIP entry %s: %+v", name, found)
			}
		}
		if len(found) != 3 || len(zr.File) != 3 {
			t.Fatalf("ZIP included files outside manifest: %+v", found)
		}
	}
	verifyZIP()

	// Missing media must not produce an HTTP 200 ZIP containing only HTML.
	if err := os.Remove(clip); err != nil {
		t.Fatal(err)
	}
	for _, check := range []string{"media missing", "media symlinked"} {
		if check == "media symlinked" {
			if err := os.Symlink(secret, clip); err != nil {
				t.Fatal(err)
			}
		}
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, reqAs("GET", path, "", owner))
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s: got HTTP %d, want 409", check, rec.Code)
		}
		if strings.HasPrefix(rec.Body.String(), "PK") || rec.Header().Get("Content-Type") == "application/zip" {
			t.Fatalf("%s: invalid ZIP response headers", check)
		}
	}
	if err := os.Remove(clip); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clip, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	verifyZIP()

	// Unsafe names and oversized lists fail closed, not silently skipped.
	manifest.Assets[0].Filename = "../private-secret"
	if err := writeJSON(filepath.Join(base, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	testArchiveResponse(t, app, owner, "GET", path, "", http.StatusConflict)
	manifest.Assets = make([]ArchiveAsset, 5001)
	for i := range manifest.Assets {
		manifest.Assets[i] = ArchiveAsset{Filename: "clip.txt"}
	}
	if err := writeJSON(filepath.Join(base, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	testArchiveResponse(t, app, owner, "GET", path, "", http.StatusConflict)
}

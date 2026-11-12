package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestOpenArchiveRegularRejectsSymlinksAndSpecialFiles(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "library", "owner", "creator", "post")
	media := filepath.Join(base, "media")
	if err := os.MkdirAll(media, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(media, "good.mp4"), []byte("video-content"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := openArchiveRegular(base, "media/good.mp4")
	if err != nil {
		t.Fatalf("valid media rejected: %v", err)
	}
	file.Close()
	private := filepath.Join(root, "private-secret")
	if err := os.WriteFile(private, []byte("NEVER_EXPOSE_THIS_TOKEN"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, filepath.Join(media, "secret.mp4")); err != nil {
		t.Fatal(err)
	}
	if f, err := openArchiveRegular(base, "media/secret.mp4"); err == nil {
		f.Close()
		t.Fatal("symlink to private file was opened")
	}
	if err := os.Symlink(private, filepath.Join(base, "index.html")); err != nil {
		t.Fatal(err)
	}
	if f, err := openArchiveRegular(base, "index.html"); err == nil {
		f.Close()
		t.Fatal("symlinked HTML was opened")
	}
	for _, path := range []string{"../private-secret", "media/../../private-secret",
		"media\\secret.mp4", "media/.", "media/good.mp4/extra"} {
		if f, err := openArchiveRegular(base, path); err == nil {
			f.Close()
			t.Fatalf("unsafe archive path accepted: %q", path)
		}
	}
	fifo := filepath.Join(media, "pipe.mp4")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	if f, err := openArchiveRegular(base, "media/pipe.mp4"); err == nil {
		f.Close()
		t.Fatal("FIFO was opened as a media file")
	}
	if err := os.Rename(media, filepath.Join(base, "media-original")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "media-original"), media); err != nil {
		t.Fatal(err)
	}
	if f, err := openArchiveRegular(base, "media/good.mp4"); err == nil {
		f.Close()
		t.Fatal("symlinked parent directory was followed")
	}
}

func TestArchiveHTTPNoFollowAndOwnerIsolation(t *testing.T) {
	root := t.TempDir()
	app, err := New(Config{
		AdminUser: "admin", AdminPassword: "strong-password-123",
		SecretKey: testKey(t), DataDir: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	owner := testLogin(t, app, "admin", "strong-password-123")
	member := testMember(t, app, "memberlocal")
	job := Job{
		ID: strings.Repeat("7", 24), OwnerID: "admin",
		Author: "creator", PostID: "post-one", Title: "Archived",
		State: "done", CreatedAt: time.Now().UTC(),
	}
	if err := app.store.upsert(job); err != nil {
		t.Fatal(err)
	}
	base := app.store.archiveDir(job.OwnerID, job.Author, job.PostID)
	historyID := strings.Repeat("a", 24)
	revBase := filepath.Join(app.store.revisionsDir(job), historyID)
	for _, dir := range []string{base, revBase} {
		if err := os.MkdirAll(filepath.Join(dir, "media"), 0700); err != nil {
			t.Fatal(err)
		}
		manifest := ArchiveManifest{
			Author: job.Author, PostID: job.PostID, Title: job.Title,
			CapturedAt: job.CreatedAt,
			Assets:     []ArchiveAsset{{Filename: "clip.mp4", Type: "video", Size: 8}},
		}
		if err := writeJSON(filepath.Join(dir, "manifest.json"), manifest); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>Private</h1>"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "media", "clip.mp4"), []byte("abcdefgh"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	mediaURL := "/library/" + job.ID + "/media/clip.mp4"
	revURL := "/library/" + job.ID + "/revisions/" + historyID + "/media/clip.mp4"
	testArchiveResponse(t, app, member, "GET", mediaURL, "", 404)
	testArchiveResponse(t, app, member, "GET", revURL, "", 404)
	testArchiveResponse(t, app, owner, "PUT", "/api/jobs/"+job.ID+"/visibility",
		`{"everyone":true}`, 200)
	req := reqAs("GET", mediaURL, "", member)
	req.Header.Set("Range", "bytes=2-5")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "cdef" {
		t.Fatalf("Range response was altered: status %d, body %q", rec.Code, rec.Body.String())
	}
	testArchiveResponse(t, app, owner, "GET", revURL, "", 200)
	testArchiveResponse(t, app, member, "GET", revURL, "", 404)

	secret := filepath.Join(root, "private-secret")
	if err := os.WriteFile(secret, []byte("NEVER_EXPOSE_THIS_TOKEN"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{base, revBase} {
		target := filepath.Join(dir, "media", "clip.mp4")
		if err := os.Remove(target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, target); err != nil {
			t.Fatal(err)
		}
	}
	testArchiveResponse(t, app, owner, "GET", mediaURL, "", 404)
	testArchiveResponse(t, app, member, "GET", mediaURL, "", 404)
	testArchiveResponse(t, app, owner, "GET", revURL, "", 404)
	testArchiveResponse(t, app, member, "GET", revURL, "", 404)
	response := testArchiveResponse(t, app, member, "GET",
		"/api/jobs/"+job.ID+"/zip", "", 500)
	if strings.Contains(string(response), "NEVER_EXPOSE_THIS_TOKEN") {
		t.Fatal("private file content leaked in ZIP error")
	}
	testArchiveResponse(t, app, owner, "PUT", "/api/jobs/"+job.ID+"/visibility",
		`{"everyone":false}`, 200)
	testArchiveResponse(t, app, member, "GET", mediaURL, "", 404)
}

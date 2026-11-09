package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testArchiveResponse(t *testing.T, app *App, cookie *http.Cookie, method, path, data string, want int) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, reqAs(method, path, data, cookie))
	if rec.Code != want {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, rec.Code, want, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func testMember(t *testing.T, app *App, name string) *http.Cookie {
	t.Helper()
	if err := app.accounts.setRegistration(true); err != nil {
		t.Fatal(err)
	}
	payload := `{"username":"` + name + `","password":"strong-user-password-123"}`
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, reqAs("POST", "/api/account/register", payload, nil))
	if rec.Code != 201 {
		t.Fatalf("register %d: %s", rec.Code, rec.Body.String())
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie
		}
	}
	t.Fatal("no member session")
	return nil
}

func TestSharedEveryoneSearchRevocationAndArchiveDate(t *testing.T) {
	root := t.TempDir()
	app, err := New(Config{AdminUser: "admin", AdminPassword: "strong-password-123", SecretKey: testKey(t), DataDir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	owner := testLogin(t, app, "admin", "strong-password-123")
	alice := testMember(t, app, "memberalice")
	bob := testMember(t, app, "memberbob")

	captured := time.Date(2026, 11, 10, 10, 15, 0, 0, time.UTC)
	job := Job{ID: "1af051d760ef831bc993a119", OwnerID: "admin", Title: "First video post", Author: "creator_name", PostID: "post-1", State: "done", URL: "https://boosty.to/creator_name/posts/post-1", CreatedAt: captured.Add(-time.Hour), ArchiveUpdatedAt: captured}
	if err = app.store.upsert(job); err != nil {
		t.Fatal(err)
	}
	folder := app.store.archiveDir(job.OwnerID, job.Author, job.PostID)
	if err = os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(folder, "manifest.json"), ArchiveManifest{Author: job.Author, Title: job.Title, CapturedAt: captured, Assets: []ArchiveAsset{{Filename: "video.mp4", Type: "ok_video", Title: "video", Size: 5}}}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(folder, "index.html"), []byte("<h1>Archived post</h1>"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(folder, "media"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(folder, "media", "video.mp4"), []byte("video"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(folder, "post.json"), []byte(`{"signedQuery":"THIS_MUST_NEVER_LEAK"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(folder, "media", "pending.part"), []byte("unfinished"), 0600); err != nil {
		t.Fatal(err)
	}

	id := "/api/jobs/" + job.ID
	testArchiveResponse(t, app, alice, "GET", "/api/shared", "", 200)
	for _, path := range []string{"/api/jobs/" + job.ID + "/manifest", "/api/jobs/" + job.ID + "/zip", "/library/" + job.ID + "/index.html"} {
		testArchiveResponse(t, app, alice, "GET", path, "", 404)
	}
	testArchiveResponse(t, app, alice, "PUT", id+"/visibility", `{"everyone":true}`, 404)
	testArchiveResponse(t, app, owner, "PUT", id+"/visibility", `{"everyone":true}`, 200)
	visible := testArchiveResponse(t, app, alice, "GET", "/api/shared?author=CREATOR", "", 200)
	var entries []struct {
		Job
		OwnerName string `json:"ownerName"`
	}
	if err = json.Unmarshal(visible, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ArchiveUpdatedAt != captured || entries[0].OwnerName != "admin" || !entries[0].SharedForAll {
		t.Fatalf("incorrect search result: %+v", entries)
	}
	for _, path := range []string{"/api/shared?author=unrelated", "/api/shared?q=unrelated", "/api/shared?q=unrelated&author=creator"} {
		b := testArchiveResponse(t, app, alice, "GET", path, "", 200)
		if strings.TrimSpace(string(b)) != "[]" {
			t.Fatalf("must filter shared jobs, got %s", b)
		}
	}
	for _, path := range []string{"/api/shared?q=FIRST", "/api/shared?q=ADMIN"} {
		b := testArchiveResponse(t, app, alice, "GET", path, "", 200)
		if !strings.Contains(string(b), job.ID) {
			t.Fatalf("shared lookup failed: %s", b)
		}
	}
	testArchiveResponse(t, app, nil, "GET", "/api/shared?author=creator", "", 401)
	zipBytes := testArchiveResponse(t, app, alice, "GET", id+"/zip", "", 200)
	zr, zerr := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if zerr != nil {
		t.Fatal(zerr)
	}
	zipped := map[string]bool{}
	for _, f := range zr.File {
		zipped[f.Name] = true
	}
	for _, name := range []string{"index.html", "manifest.json", "media/video.mp4"} {
		if !zipped[name] {
			t.Fatalf("missing portable archive file %s: %+v", name, zipped)
		}
	}
	for _, name := range []string{"post.json", "media/pending.part"} {
		if zipped[name] {
			t.Fatalf("unsafe archive file included: %s", name)
		}
	}
	for _, path := range []string{id + "/manifest", "/library/" + job.ID + "/"} {
		testArchiveResponse(t, app, bob, "GET", path, "", 200)
	}
	testArchiveResponse(t, app, owner, "PUT", id+"/visibility", `{"everyone":false}`, 200)
	if j, ok := app.store.get(job.ID); !ok || !j.ArchiveUpdatedAt.Equal(captured) {
		t.Fatal("sharing mutation changed archive timestamp")
	}
	testArchiveResponse(t, app, bob, "GET", id+"/manifest", "", 404)
	testArchiveResponse(t, app, owner, "POST", id+"/shares", `{"username":"memberalice"}`, 200)
	testArchiveResponse(t, app, alice, "GET", id+"/manifest", "", 200)
	testArchiveResponse(t, app, bob, "GET", id+"/manifest", "", 404)
	// Settings survive reload while each user's content remains isolated.
	restored, err := newStorage(root, app.cfg.SecretKey)
	if err != nil {
		t.Fatal(err)
	}
	j, ok := restored.get(job.ID)
	if !ok || j.SharedForAll || len(j.SharedWith) != 1 || !j.ArchiveUpdatedAt.Equal(captured) {
		t.Fatalf("persistence or timestamp mismatch: %+v", j)
	}
	testArchiveResponse(t, app, owner, "DELETE", id+"/shares", `{"username":"memberalice"}`, 200)
	testArchiveResponse(t, app, alice, "GET", id+"/manifest", "", 404)
	testArchiveResponse(t, app, bob, "GET", "/api/shared", "", 200)
}

func TestArchiveTimestampMigration(t *testing.T) {
	dir := t.TempDir()
	key := testKey(t)
	store, err := newStorage(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	captured := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	job := Job{ID: "0123456789abcdef01234567", OwnerID: "admin", Author: "creator", PostID: "test", State: "done", CreatedAt: captured.Add(-time.Hour)}
	if err = store.upsert(job); err != nil {
		t.Fatal(err)
	}
	folder := store.archiveDir(job.OwnerID, job.Author, job.PostID)
	if err = os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(filepath.Join(folder, "manifest.json"), ArchiveManifest{CapturedAt: captured}); err != nil {
		t.Fatal(err)
	}
	restored, err := newStorage(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	if err = restored.migrateArchiveTimestamps(); err != nil {
		t.Fatal(err)
	}
	item, _ := restored.get(job.ID)
	if !item.ArchiveUpdatedAt.Equal(captured) {
		t.Fatalf("want %s got %s", captured, item.ArchiveUpdatedAt)
	}
}

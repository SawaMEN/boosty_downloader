package server

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteCutoverACLHTTPRoundTrip(t *testing.T) {
	cfg, backup, snapshot := sqlActivationFixture(t)
	if err := ActivateSQLitePrimaryExperimental(cfg.DataDir, backup, snapshot); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BD_SQLITE_PRIMARY", "1")
	t.Setenv("BD_SQLITE_SHADOW", "0")
	t.Setenv("BD_SQLITE_VERIFY", "0")
	t.Setenv("BD_SQLITE_MEDIA_SHADOW", "0")
	t.Setenv("BD_SQLITE_CATALOG_READS", "0")
	id := strings.Repeat("d", 24)
	api := "/api/jobs/" + id
	media := "/library/" + id + "/media/movie.mp4"
	source := filepath.Join(cfg.DataDir, "library", "admin", "creator", "post-a", "media", "movie.mp4")
	original, err := os.ReadFile(source)
	if err != nil || string(original) != "original source media" {
		t.Fatalf("fixture media: %v", err)
	}
	open := func() *App {
		t.Helper()
		app, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(app.Close)
		return app
	}
	assertAccess := func(app *App, cookie *http.Cookie, allowed bool) {
		t.Helper()
		want := 404
		if allowed {
			want = 200
		}
		body := testArchiveResponse(t, app, cookie, "GET", media, "", want)
		if allowed && !bytes.Equal(body, original) {
			t.Fatal("archive bytes changed across backend transition")
		}
		testArchiveResponse(t, app, cookie, "GET", api+"/manifest", "", want)
	}
	app := open()
	if app.store.primary == nil {
		t.Fatal("SQLite not active")
	}
	owner := testLogin(t, app, "admin", "strong-password-123")
	alice := testMember(t, app, "cutoveralice")
	bob := testMember(t, app, "cutoverbob")
	assertAccess(app, owner, true)
	assertAccess(app, alice, false)
	assertAccess(app, bob, false)
	testArchiveResponse(t, app, owner, "POST", api+"/shares", `{"username":"cutoveralice"}`, 200)
	assertAccess(app, alice, true)
	assertAccess(app, bob, false)
	app.Close()

	app = open()
	owner = testLogin(t, app, "admin", "strong-password-123")
	alice = testLogin(t, app, "cutoveralice", "strong-user-password-123")
	bob = testLogin(t, app, "cutoverbob", "strong-user-password-123")
	assertAccess(app, alice, true)
	assertAccess(app, bob, false)
	testArchiveResponse(t, app, owner, "DELETE", api+"/shares", `{"username":"cutoveralice"}`, 200)
	testArchiveResponse(t, app, owner, "POST", api+"/shares", `{"username":"cutoverbob"}`, 200)
	assertAccess(app, alice, false)
	assertAccess(app, bob, true)
	testArchiveResponse(t, app, nil, "GET", media, "", 401)
	app.Close()

	full := filepath.Join(t.TempDir(), "rollback-full")
	if err := PrepareFullDataSnapshot(cfg.DataDir, full); err != nil {
		t.Fatal(err)
	}
	if err := RollbackSQLiteToJSON(cfg.DataDir, full); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BD_SQLITE_PRIMARY", "0")
	app = open()
	if app.store.primary != nil {
		t.Fatal("JSON rollback still using SQLite")
	}
	owner = testLogin(t, app, "admin", "strong-password-123")
	alice = testLogin(t, app, "cutoveralice", "strong-user-password-123")
	bob = testLogin(t, app, "cutoverbob", "strong-user-password-123")
	assertAccess(app, owner, true)
	assertAccess(app, alice, false)
	assertAccess(app, bob, true)
	testArchiveResponse(t, app, nil, "GET", media, "", 401)
	testArchiveResponse(t, app, owner, "DELETE", api+"/shares", `{"username":"cutoverbob"}`, 200)
	assertAccess(app, bob, false)
	app.Close()

	app = open()
	owner = testLogin(t, app, "admin", "strong-password-123")
	alice = testLogin(t, app, "cutoveralice", "strong-user-password-123")
	bob = testLogin(t, app, "cutoverbob", "strong-user-password-123")
	assertAccess(app, owner, true)
	assertAccess(app, alice, false)
	assertAccess(app, bob, false)
	app.Close()
	if err := VerifySQLiteMetadataBackup(backup); err != nil {
		t.Fatal("original metadata backup changed", err)
	}
	for _, dir := range []string{snapshot, full} {
		if err := VerifyFullDataSnapshot(dir); err != nil {
			t.Fatal("recovery point changed", err)
		}
	}
}

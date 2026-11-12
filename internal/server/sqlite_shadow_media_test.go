package server

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sqliteMediaShadowApp(t *testing.T) (*App, string, string, string, string) {
	t.Helper()
	root, jobID, historyID, media := sqliteSnapshotAuditFixture(t)
	t.Setenv("BD_SQLITE_VERIFY", "")
	t.Setenv("BD_SQLITE_SHADOW", "1")
	t.Setenv("BD_SQLITE_MEDIA_SHADOW", "1")
	app, err := New(Config{
		DataDir: root, SecretKey: testKey(t),
		AdminUser: "admin", AdminPassword: "strong-password-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app, root, jobID, historyID, media
}

func sqliteMediaRevisionCount(t *testing.T, app *App) int {
	t.Helper()
	var count int
	if err := app.store.shadow.meta.db.QueryRow("SELECT COUNT(*) FROM archive_revisions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestSQLiteMediaShadowMirrorsHistoryAndKeepsOwnerACL(t *testing.T) {
	app, root, jobID, oldID, media := sqliteMediaShadowApp(t)
	if err := app.store.syncSQLiteShadow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count := sqliteMediaRevisionCount(t, app); count != 2 {
		t.Fatalf("existing current and historical revisions not indexed: %d", count)
	}
	newID := strings.Repeat("c", 24)
	sqliteReindexCopyHistory(t, root, oldID, newID, media)
	app.store.notifySQLiteMedia()
	if !app.store.sqliteShadowStatus().MediaPending {
		t.Fatal("on-disk revision change was not marked pending")
	}
	if err := app.store.syncSQLiteShadow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if count := sqliteMediaRevisionCount(t, app); count != 3 {
		t.Fatalf("new historical snapshot not indexed: %d", count)
	}
	if status := app.store.sqliteShadowStatus(); status.MediaPending || status.MediaError != "" {
		t.Fatalf("media mirror did not catch up: %+v", status)
	}
	var owner string
	if err := app.store.shadow.meta.db.QueryRow(`SELECT a.owner_id FROM archive_revisions r
		JOIN owner_archives a ON a.archive_id=r.archive_id
		WHERE r.revision_id=? AND a.archive_id=?`, newID, jobID).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "first" {
		t.Fatal("media shadow assigned the history to another account")
	}
	if _, err := app.store.shadow.meta.job(context.Background(), "second", jobID); err != sql.ErrNoRows {
		t.Fatalf("owner isolation failed: %v", err)
	}
}

func TestSQLiteMediaShadowDefersActiveDownload(t *testing.T) {
	app, _, jobID, _, _ := sqliteMediaShadowApp(t)
	if err := app.store.syncSQLiteShadow(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, ok := app.store.get(jobID)
	if !ok {
		t.Fatal("archive not found")
	}
	job.State = "running"
	if err := app.store.upsert(job); err != nil {
		t.Fatal(err)
	}
	app.store.notifySQLiteMedia()
	if err := app.store.syncSQLiteShadow(context.Background()); err != nil {
		t.Fatalf("media shadow must defer, not fail, while download runs: %v", err)
	}
	if !app.store.sqliteShadowStatus().MediaPending {
		t.Fatal("in-flight work incorrectly marked as mirrored")
	}
	job.State = "failed"
	if err := app.store.upsert(job); err != nil {
		t.Fatal(err)
	}
	app.store.notifySQLiteMedia()
	if err := app.store.syncSQLiteShadow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if app.store.sqliteShadowStatus().MediaPending {
		t.Fatal("completed work did not clear pending media mirror")
	}
}

func TestSQLiteMediaShadowPreservesSQLOnBadHistory(t *testing.T) {
	app, root, _, oldID, media := sqliteMediaShadowApp(t)
	if err := app.store.syncSQLiteShadow(context.Background()); err != nil {
		t.Fatal(err)
	}
	copyID := strings.Repeat("d", 24)
	sqliteReindexCopyHistory(t, root, oldID, copyID, media)
	file := filepath.Join(root, "revisions", "first", "creator", "post-1", copyID, "media", media)
	if err := os.WriteFile(file, []byte("corrupted history video"), 0600); err != nil {
		t.Fatal(err)
	}
	app.store.notifySQLiteMedia()
	if err := app.store.syncSQLiteShadow(context.Background()); err == nil {
		t.Fatal("corrupt historical media accepted")
	}
	if count := sqliteMediaRevisionCount(t, app); count != 2 {
		t.Fatalf("failed index update left a partial SQL result: %d", count)
	}
	if !app.store.sqliteShadowStatus().MediaPending || app.store.sqliteShadowStatus().MediaError == "" {
		t.Fatal("media mirror failure was not reported")
	}
	// The live owner metadata is still served from authoritative JSON.
	job, ok := app.store.get(strings.Repeat("1", 24))
	if !ok || job.OwnerID != "first" {
		t.Fatal("damaged SQLite projection changed archive ownership")
	}
}

func TestSQLiteMediaShadowSQLTriggerRollbackAndRetry(t *testing.T) {
	app, root, _, oldID, media := sqliteMediaShadowApp(t)
	ctx := context.Background()
	if err := app.store.syncSQLiteShadow(ctx); err != nil {
		t.Fatal(err)
	}
	newID := strings.Repeat("e", 24)
	sqliteReindexCopyHistory(t, root, oldID, newID, media)
	if _, err := app.store.shadow.meta.db.Exec(`CREATE TRIGGER refuse_media_shadow
		BEFORE INSERT ON archive_revisions
		BEGIN SELECT RAISE(ABORT, 'simulated disk failure'); END`); err != nil {
		t.Fatal(err)
	}
	app.store.notifySQLiteMedia()
	if err := app.store.syncSQLiteShadow(ctx); err == nil {
		t.Fatal("SQL trigger failure was ignored")
	}
	if count := sqliteMediaRevisionCount(t, app); count != 2 {
		t.Fatalf("failed SQL transaction was not rolled back: %d", count)
	}
	if _, err := app.store.shadow.meta.db.Exec("DROP TRIGGER refuse_media_shadow"); err != nil {
		t.Fatal(err)
	}
	if err := app.store.syncSQLiteShadow(ctx); err != nil {
		t.Fatal(err)
	}
	if count := sqliteMediaRevisionCount(t, app); count != 3 {
		t.Fatalf("shadow did not retry transaction after SQL recovered: %d", count)
	}
	if app.store.sqliteShadowStatus().MediaError != "" {
		t.Fatal("successful retry did not clear the media shadow error")
	}
}

func TestSQLiteMediaShadowRequiresMetadataShadow(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BD_SQLITE_SHADOW", "")
	t.Setenv("BD_SQLITE_MEDIA_SHADOW", "1")
	app, err := New(Config{DataDir: root, SecretKey: testKey(t),
		AdminUser: "admin", AdminPassword: "strong-password-123"})
	if err == nil {
		app.Close()
		t.Fatal("media mirror without metadata shadow was accepted")
	}
}

func TestSQLiteMediaShadowOffByDefault(t *testing.T) {
	root, _, _, _ := sqliteSnapshotAuditFixture(t)
	t.Setenv("BD_SQLITE_SHADOW", "1")
	t.Setenv("BD_SQLITE_MEDIA_SHADOW", "")
	t.Setenv("BD_SQLITE_VERIFY", "")
	app, err := New(Config{DataDir: root, SecretKey: testKey(t),
		AdminUser: "admin", AdminPassword: "strong-password-123"})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	status := app.store.sqliteShadowStatus()
	if status.MediaEnabled || status.MediaPending {
		t.Fatal("media indexing unexpectedly enabled by default")
	}
}

func TestSQLiteMediaShadowDoesNotSilentlyDropMissingCurrentArchive(t *testing.T) {
	app, root, _, _, _ := sqliteMediaShadowApp(t)
	ctx := context.Background()
	if err := app.store.syncSQLiteShadow(ctx); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(root, "library", "first", "creator", "post-1", "manifest.json")
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	app.store.notifySQLiteMedia()
	if err := app.store.syncSQLiteShadow(ctx); err == nil {
		t.Fatal("existing SQL current revision was deleted after a missing manifest")
	}
	if count := sqliteMediaRevisionCount(t, app); count != 2 {
		t.Fatalf("missing manifest changed SQL revision inventory: %d", count)
	}
	if !app.store.sqliteShadowStatus().MediaPending {
		t.Fatal("SQL media state was marked consistent after missing live data")
	}
}

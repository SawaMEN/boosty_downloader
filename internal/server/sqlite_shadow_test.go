package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sqliteShadowApp(t *testing.T) (*App, string) {
	t.Helper()
	root, _ := sqliteCanaryFixture(t)
	t.Setenv("BD_SQLITE_SHADOW", "1")
	t.Setenv("BD_SQLITE_VERIFY", "")
	app, err := New(Config{
		DataDir: root, SecretKey: testKey(t),
		AdminUser: "admin", AdminPassword: "strong-password-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	return app, root
}

func TestSQLiteShadowMirrorsJobsImportsAndPrivateGrants(t *testing.T) {
	app, _ := sqliteShadowApp(t)
	ctx := context.Background()
	first := strings.Repeat("1", 24)
	second := strings.Repeat("2", 24)
	before, ok := app.store.get(first)
	if !ok {
		t.Fatal("missing owner job")
	}
	changed := before
	changed.Title = "JSON is authoritative"
	changed.SharedWith = []string{"third"}
	changed.SharedForAll = true
	changed.AutoSync = true
	if err := app.store.upsert(changed); err != nil {
		t.Fatal(err)
	}
	importID := strings.Repeat("a", 24)
	records := app.store.importsFor("first")
	if len(records) != 1 {
		t.Fatal("expected imported author fixture")
	}
	imp := records[0]
	imp.AutoSync = true
	if err := app.store.updateImport(imp); err != nil {
		t.Fatal(err)
	}
	if err := app.store.syncSQLiteShadow(ctx); err != nil {
		t.Fatal(err)
	}
	sqlJob, err := app.store.shadow.meta.job(ctx, "first", first)
	if err != nil || sqlJob.Title != "JSON is authoritative" ||
		!sqlJob.SharedForAll || !sqlJob.AutoSync ||
		len(sqlJob.SharedWith) != 1 || sqlJob.SharedWith[0] != "third" {
		t.Fatalf("shadow archive differs: %+v %v", sqlJob, err)
	}
	if _, err := app.store.shadow.meta.job(ctx, "second", first); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("shadow leaked first owner's job: %v", err)
	}
	if _, err := app.store.shadow.meta.job(ctx, "second", second); err != nil {
		t.Fatal("second owner's independent job unavailable:", err)
	}
	imported, err := app.store.shadow.meta.authorImport(ctx, "first", importID)
	if err != nil || !imported.AutoSync {
		t.Fatalf("shadow import differs: %+v %v", imported, err)
	}
	var grants int
	if err := app.store.shadow.meta.db.QueryRow(
		"SELECT COUNT(*) FROM archive_grants WHERE archive_id=? AND grantee_owner_id='third'", first).Scan(&grants); err != nil || grants != 1 {
		t.Fatalf("new grant not reflected: %d %v", grants, err)
	}
	var oldGrants int
	if err := app.store.shadow.meta.db.QueryRow(
		"SELECT COUNT(*) FROM archive_grants WHERE archive_id=? AND grantee_owner_id='second'", first).Scan(&oldGrants); err != nil || oldGrants != 0 {
		t.Fatalf("revoked grant remained: %d %v", oldGrants, err)
	}
	if status := app.store.sqliteShadowStatus(); status.LastError != "" || status.LastSuccess.IsZero() {
		t.Fatalf("shadow status not healthy: %+v", status)
	}
}

func TestSQLiteShadowCanRecoverAfterSQLiteFailureWithoutLosingJSON(t *testing.T) {
	app, root := sqliteShadowApp(t)
	ctx := context.Background()
	id := strings.Repeat("1", 24)
	if _, err := app.store.shadow.meta.db.Exec(`
		CREATE TRIGGER block_shadow_archive BEFORE UPDATE ON owner_archives
		BEGIN SELECT RAISE(ABORT, 'simulated SQLite outage'); END`); err != nil {
		t.Fatal(err)
	}
	before, ok := app.store.get(id)
	if !ok {
		t.Fatal("fixture missing job")
	}
	after := before
	after.Title = "Persisted in JSON despite SQL outage"
	if err := app.store.upsert(after); err != nil {
		t.Fatalf("SQL outage must not undo the authoritative JSON write: %v", err)
	}
	if err := app.store.syncSQLiteShadow(ctx); err == nil {
		t.Fatal("SQL failure was silently swallowed")
	}
	gotJSON, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var records map[string]Job
	if err := json.Unmarshal(gotJSON, &records); err != nil || records[id].Title != after.Title {
		t.Fatal("SQL failure lost authoritative JSON data")
	}
	oldSQL, err := app.store.shadow.meta.job(ctx, "first", id)
	if err != nil || oldSQL.Title == after.Title {
		t.Fatalf("failed SQL transaction left a partial projection: %+v %v", oldSQL, err)
	}
	if status := app.store.sqliteShadowStatus(); status.LastError == "" {
		t.Fatal("shadow failure was not reported")
	}
	if _, err := app.store.shadow.meta.db.Exec("DROP TRIGGER block_shadow_archive"); err != nil {
		t.Fatal(err)
	}
	if err := app.store.syncSQLiteShadow(ctx); err != nil {
		t.Fatal("shadow did not recover from a transient SQL outage:", err)
	}
	recovered, err := app.store.shadow.meta.job(ctx, "first", id)
	if err != nil || recovered.Title != after.Title {
		t.Fatalf("shadow did not catch up: %+v %v", recovered, err)
	}
	if app.store.sqliteShadowStatus().LastError != "" {
		t.Fatal("shadow error was not cleared after recovery")
	}
}

func TestSQLiteShadowRejectsForgedSQLGrantAndPreservesJSONAuthority(t *testing.T) {
	app, _ := sqliteShadowApp(t)
	ctx := context.Background()
	id := strings.Repeat("1", 24)
	if _, err := app.store.shadow.meta.db.Exec(
		"INSERT INTO archive_grants (archive_id,grantee_owner_id,created_at) VALUES (?,?,?)",
		id, "forged", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := app.store.syncSQLiteShadow(ctx); err == nil {
		t.Fatal("forged SQL-only grant was silently accepted")
	}
	job, ok := app.store.get(id)
	if !ok {
		t.Fatal("JSON job missing")
	}
	for _, user := range job.SharedWith {
		if user == "forged" {
			t.Fatal("SQL grant changed JSON authorization")
		}
	}
	if _, err := app.store.shadow.meta.db.Exec(
		"DELETE FROM archive_grants WHERE archive_id=? AND grantee_owner_id='forged'", id); err != nil {
		t.Fatal(err)
	}
	if err := app.store.syncSQLiteShadow(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteShadowMirrorsArchiveTrashAndPurge(t *testing.T) {
	app, _ := sqliteShadowApp(t)
	ctx := context.Background()
	id := strings.Repeat("1", 24)
	before, ok := app.store.get(id)
	if !ok {
		t.Fatal("missing fixture job")
	}
	now := time.Now().UTC()
	trashed := before
	trashed.DeletedAt = &now
	trashed.AutoSync = false
	trashed.SharedWith = nil
	trashed.SharedForAll = false
	if err := app.store.upsert(trashed); err != nil {
		t.Fatal(err)
	}
	if err := app.store.syncSQLiteShadow(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.shadow.meta.job(ctx, "first", id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("trashed SQL job remained visible: %v", err)
	}
	var grants int
	if err := app.store.shadow.meta.db.QueryRow(
		"SELECT COUNT(*) FROM archive_grants WHERE archive_id=?", id).Scan(&grants); err != nil || grants != 0 {
		t.Fatal("trash did not revoke SQL grants", grants, err)
	}
	app.store.mu.Lock()
	delete(app.store.jobs, id)
	err := app.store.saveJobsLocked()
	app.store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.syncSQLiteShadow(ctx); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := app.store.shadow.meta.db.QueryRow(
		"SELECT COUNT(*) FROM owner_archives WHERE archive_id=?", id).Scan(&count); err != nil || count != 0 {
		t.Fatal("purged archive retained in SQL", count, err)
	}
}

func TestSQLiteShadowColdStartReconcilesJSONWrittenAfterExport(t *testing.T) {
	root, jobs := sqliteCanaryFixture(t)
	id := strings.Repeat("1", 24)
	j := jobs[id]
	j.Title = "offline JSON changed"
	jobs[id] = j
	if err := writeJSON(filepath.Join(root, "jobs.json"), jobs); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BD_SQLITE_SHADOW", "1")
	t.Setenv("BD_SQLITE_VERIFY", "")
	app, err := New(Config{
		DataDir: root, SecretKey: testKey(t),
		AdminUser: "admin", AdminPassword: "strong-password-123",
	})
	if err != nil {
		t.Fatalf("shadow should recover from JSON ahead of SQL at startup: %v", err)
	}
	defer app.Close()
	got, err := app.store.shadow.meta.job(context.Background(), "first", id)
	if err != nil || got.Title != j.Title {
		t.Fatalf("SQL did not catch up: %+v %v", got, err)
	}
}

func TestSQLiteShadowExplicitOptInAndExistingDBRequired(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BD_SQLITE_SHADOW", "1")
	t.Setenv("BD_SQLITE_VERIFY", "")
	app, err := New(Config{
		DataDir: root, SecretKey: testKey(t),
		AdminUser: "admin", AdminPassword: "strong-password-123",
	})
	if err == nil {
		app.Close()
		t.Fatal("shadow mode silently created an unverified SQLite database")
	}
	if _, err := os.Lstat(filepath.Join(root, "metadata.db")); !os.IsNotExist(err) {
		t.Fatal("missing metadata.db was created unexpectedly")
	}
	t.Setenv("BD_SQLITE_VERIFY", "1")
	app, err = New(Config{
		DataDir: root, SecretKey: testKey(t),
		AdminUser: "admin", AdminPassword: "strong-password-123",
	})
	if err == nil {
		app.Close()
		t.Fatal("shadow+preflight conflict accepted")
	}
}

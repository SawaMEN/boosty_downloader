package server

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sqliteCanaryFixture(t *testing.T) (string, map[string]Job) {
	t.Helper()
	root := t.TempDir()
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	jobs := map[string]Job{
		strings.Repeat("1", 24): {
			ID: strings.Repeat("1", 24), OwnerID: "first",
			Author: "creator", PostID: "post-1", State: "failed",
			SharedWith: []string{"second"}, CreatedAt: now, UpdatedAt: now,
		},
		strings.Repeat("2", 24): {
			ID: strings.Repeat("2", 24), OwnerID: "second",
			Author: "creator", PostID: "post-1", State: "failed",
			CreatedAt: now, UpdatedAt: now,
		},
	}
	if err := writeJSON(filepath.Join(root, "jobs.json"), jobs); err != nil {
		t.Fatal(err)
	}
	imp := AuthorImport{
		ID: strings.Repeat("a", 24), OwnerID: "first", Author: "creator",
		State: "done", UpdatedAt: now, CreatedAt: now,
	}
	if err := writeJSON(filepath.Join(root, "imports.json"), map[string]AuthorImport{imp.ID: imp}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "../../tools/sqlite_migrate.py",
		"--data-dir", root, "--output", filepath.Join(root, "metadata.db"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture export: %v: %s", err, output)
	}
	return root, jobs
}

func TestSQLitePreflightAcceptsOfflineExportAndPreservesOwners(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	if err := verifySQLiteExport(root); err != nil {
		t.Fatalf("valid offline export rejected: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var posts, owners int
	if err := db.QueryRow("SELECT count(*) FROM source_posts").Scan(&posts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT count(*) FROM owner_archives").Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || owners != 2 {
		t.Fatalf("independent owners unexpectedly merged: posts=%d archives=%d", posts, owners)
	}
}

func TestSQLitePreflightRejectsStaleJSONAndIncorrectGrants(t *testing.T) {
	root, jobs := sqliteCanaryFixture(t)
	j := jobs[strings.Repeat("1", 24)]
	j.SharedWith = nil
	jobs[j.ID] = j
	if err := writeJSON(filepath.Join(root, "jobs.json"), jobs); err != nil {
		t.Fatal(err)
	}
	if err := verifySQLiteExport(root); err == nil {
		t.Fatal("stale export accepted after JSON ACL change")
	}

	root, _ = sqliteCanaryFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("INSERT INTO archive_grants (archive_id,grantee_owner_id,created_at) VALUES (?,?,?)",
		strings.Repeat("2", 24), "first", time.Now().UTC().Format(time.RFC3339))
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySQLiteExport(root); err == nil {
		t.Fatal("unexpected grant in SQLite was not rejected")
	}
}

func TestSQLitePreflightRejectsMissingDatabaseAndSymlink(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	dbPath := filepath.Join(root, "metadata.db")
	if err := os.Remove(dbPath); err != nil {
		t.Fatal(err)
	}
	if err := verifySQLiteExport(root); err == nil {
		t.Fatal("missing database was accepted")
	}
	if err := os.Symlink(filepath.Join(root, "jobs.json"), dbPath); err != nil {
		t.Fatal(err)
	}
	if err := verifySQLiteExport(root); err == nil {
		t.Fatal("symlink database was accepted")
	}
}

func TestSQLitePreflightRunsBeforeServerWrites(t *testing.T) {
	root, jobs := sqliteCanaryFixture(t)
	t.Setenv("BD_SQLITE_VERIFY", "1")
	app, err := New(Config{
		DataDir: root, AdminUser: "admin", AdminPassword: "strong-password-123",
		SecretKey: testKey(t),
	})
	if err != nil {
		t.Fatalf("read-only canary blocked startup: %v", err)
	}
	app.Close()
	// A changed authorization record must be rejected before any worker starts.
	j := jobs[strings.Repeat("1", 24)]
	j.SharedForAll = true
	jobs[j.ID] = j
	if err := writeJSON(filepath.Join(root, "jobs.json"), jobs); err != nil {
		t.Fatal(err)
	}
	if app, err := New(Config{
		DataDir: root, AdminUser: "admin", AdminPassword: "strong-password-123",
		SecretKey: testKey(t),
	}); err == nil {
		app.Close()
		t.Fatal("server started with a mismatching SQLite audit")
	}
}


func TestSQLitePreflightRejectsCrossOwnerRevisionPath(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("1", 24)
	_, err = db.Exec(`INSERT INTO archive_revisions
		(revision_id,archive_id,captured_at,title,snapshot_relpath,asset_count,is_current)
		VALUES (?,?,?,?,?,?,?)`,
		"current:"+id, id, "2026-11-11T12:00:00Z", "Wrong owner",
		"library/second/creator/post-1", 0, 1)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySQLiteExport(root); err == nil {
		t.Fatal("cross-owner revision path accepted in SQLite")
	}
}

package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sqliteReindexFixture(t *testing.T) (root, backup, jobID, historyID, media string) {
	t.Helper()
	root, jobID, historyID, media = sqliteSnapshotAuditFixture(t)
	backup = filepath.Join(t.TempDir(), "before-reindex")
	if err := PrepareSQLiteBackup(root, backup); err != nil {
		t.Fatal(err)
	}
	return root, backup, jobID, historyID, media
}

func sqliteReindexCopyHistory(t *testing.T, root, oldID, newID, media string) {
	t.Helper()
	base := filepath.Join(root, "revisions", "first", "creator", "post-1")
	source := filepath.Join(base, oldID)
	dest := filepath.Join(base, newID)
	if err := os.MkdirAll(filepath.Join(dest, "media"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"index.html", "manifest.json", filepath.Join("media", media)} {
		content, err := os.ReadFile(filepath.Join(source, part))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dest, part), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func sqliteReindexRevisionCount(t *testing.T, root string) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM archive_revisions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestSQLiteOfflineReindexAddsSnapshotAndPreservesOwnerACL(t *testing.T) {
	root, backup, jobID, oldID, media := sqliteReindexFixture(t)
	newID := strings.Repeat("c", 24)
	sqliteReindexCopyHistory(t, root, oldID, newID, media)
	if err := ReindexSQLiteRevisionInventory(root, backup); err != nil {
		t.Fatal(err)
	}
	if got := sqliteReindexRevisionCount(t, root); got != 3 {
		t.Fatalf("expected 3 indexed revisions, got %d", got)
	}
	if err := VerifySQLiteSnapshotInventory(root); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var owner, archive string
	if err := db.QueryRow(`SELECT a.owner_id,r.archive_id
		FROM archive_revisions r JOIN owner_archives a ON a.archive_id=r.archive_id
		WHERE r.revision_id=?`, newID).Scan(&owner, &archive); err != nil {
		t.Fatal(err)
	}
	if owner != "first" || archive != jobID {
		t.Fatal("history revision inherited another owner's ACL")
	}
	var grants int
	if err := db.QueryRow("SELECT COUNT(*) FROM archive_grants WHERE archive_id=?", jobID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 1 {
		t.Fatalf("revision reindex changed access grants: %d", grants)
	}
}

func TestSQLiteOfflineReindexRemovesPrunedHistoryOnlyFromSQL(t *testing.T) {
	root, backup, _, historyID, _ := sqliteReindexFixture(t)
	historyDir := filepath.Join(root, "revisions", "first", "creator", "post-1", historyID)
	if err := os.RemoveAll(historyDir); err != nil {
		t.Fatal(err)
	}
	if err := ReindexSQLiteRevisionInventory(root, backup); err != nil {
		t.Fatal(err)
	}
	if got := sqliteReindexRevisionCount(t, root); got != 1 {
		t.Fatalf("pruned history retained in SQL; count=%d", got)
	}
	if err := VerifySQLiteSnapshotInventory(root); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteOfflineReindexRejectsStaleBackupWithoutWritingSQL(t *testing.T) {
	root, backup, _, historyID, media := sqliteReindexFixture(t)
	sqliteReindexCopyHistory(t, root, historyID, strings.Repeat("c", 24), media)
	jobsPath := filepath.Join(root, "jobs.json")
	b, err := os.ReadFile(jobsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobsPath, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	before := sqliteReindexRevisionCount(t, root)
	if err := ReindexSQLiteRevisionInventory(root, backup); err == nil {
		t.Fatal("stale backup accepted for writable SQL transaction")
	}
	if got := sqliteReindexRevisionCount(t, root); got != before {
		t.Fatalf("stale backup caused SQL edits: %d -> %d", before, got)
	}
}

func TestSQLiteOfflineReindexRejectsModifiedHistoricalBytes(t *testing.T) {
	root, backup, _, historyID, media := sqliteReindexFixture(t)
	historyDir := filepath.Join(root, "revisions", "first", "creator", "post-1", historyID)
	assetPath := filepath.Join(historyDir, "media", media)
	if err := os.WriteFile(assetPath, []byte(strings.Repeat("b", 48)), 0600); err != nil {
		t.Fatal(err)
	}
	// Fixing the manifest to match a substituted historical media file does
	// not authorize changing an immutable SQL-indexed historical revision.
	manifestPath := filepath.Join(historyDir, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest ArchiveManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	digest, size, err := sqliteReindexHashFile(assetPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Assets[0].SHA256 = digest
	manifest.Assets[0].Size = size
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ReindexSQLiteRevisionInventory(root, backup); err == nil {
		t.Fatal("modified historical revision replaced the original SQL digest")
	}
	if got := sqliteReindexRevisionCount(t, root); got != 2 {
		t.Fatalf("failed historical check modified SQL rows: %d", got)
	}
}

func TestSQLiteOfflineReindexRollbackOnSQLInsertFailure(t *testing.T) {
	root, _, _, historyID, media := sqliteReindexFixture(t)
	// Install the failing trigger BEFORE backup. A backup still contains a
	// valid SQLite schema and matches the current archived metadata.
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER refuse_revision
		BEFORE INSERT ON archive_revisions
		BEGIN SELECT RAISE(ABORT, 'simulated media index failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "trigger-backup")
	if err := PrepareSQLiteBackup(root, backup); err != nil {
		t.Fatal(err)
	}
	sqliteReindexCopyHistory(t, root, historyID, strings.Repeat("d", 24), media)
	if err := ReindexSQLiteRevisionInventory(root, backup); err == nil {
		t.Fatal("SQL trigger failure was ignored")
	}
	if got := sqliteReindexRevisionCount(t, root); got != 2 {
		t.Fatalf("failed SQL transaction did not roll back: %d", got)
	}
	if _, err := sql.Open("sqlite", filepath.Join(root, "metadata.db")); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteOfflineReindexRequiresBackup(t *testing.T) {
	root, _, _, _, _ := sqliteReindexFixture(t)
	if err := ReindexSQLiteRevisionInventory(root, ""); err == nil {
		t.Fatal("writable reindex allowed without backup")
	}
	if err := ReindexSQLiteRevisionInventory(root, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("writable reindex accepted nonexistent backup")
	}
}

func TestSQLiteOfflineReindexRefusesUnsafeHistoricalSymlink(t *testing.T) {
	root, backup, _, historyID, _ := sqliteReindexFixture(t)
	historyDir := filepath.Join(root, "revisions", "first", "creator", "post-1")
	if err := os.Symlink(historyID, filepath.Join(historyDir, strings.Repeat("e", 24))); err != nil {
		t.Fatal(err)
	}
	if err := ReindexSQLiteRevisionInventory(root, backup); err == nil {
		t.Fatal("symlink to a different revision was indexed")
	}
}

func TestSQLiteOfflineReindexPreservesEmptyOwnerArchiveSet(t *testing.T) {
	root, backup, _, _, _ := sqliteReindexFixture(t)
	// Historical SQL still contains a second owner without any media files.
	if err := ReindexSQLiteRevisionInventory(root, backup); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM owner_archives WHERE owner_id='second'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("a second owner's independent archive was deleted")
	}
	if err := sqliteAuditDatabaseIntegrity(db); err != nil {
		t.Fatal(err)
	}
	_ = context.Background()
}

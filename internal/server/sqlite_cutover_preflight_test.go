package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteCutoverPreflightAcceptsConsistentOfflineRecoveryPoint(t *testing.T) {
	root, backup, _, _, _ := sqliteReindexFixture(t)
	if err := VerifySQLiteCutoverPreflight(root, backup); err != nil {
		t.Fatalf("valid SQL/JSON/filesystem recovery point rejected: %v", err)
	}
}

func TestSQLiteCutoverPreflightRejectsOldMetadataBackup(t *testing.T) {
	root, backup, _, _, _ := sqliteReindexFixture(t)
	file := filepath.Join(root, "jobs.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteCutoverPreflight(root, backup); err == nil {
		t.Fatal("preflight accepted stale recovery backup after a JSON change")
	}
}

func TestSQLiteCutoverPreflightRejectsUntrackedMediaAndIncompleteWAL(t *testing.T) {
	root, backup, _, _, _ := sqliteReindexFixture(t)
	path := filepath.Join(root, "revisions", "first", "creator", "post-1",
		"ffffffffffffffffffffffff")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "manifest.json"),
		[]byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteCutoverPreflight(root, backup); err == nil {
		t.Fatal("preflight accepted a snapshot missing from SQL")
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "metadata.db-wal"), []byte("uncheckpointed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteCutoverPreflight(root, backup); err == nil {
		t.Fatal("preflight silently ignored uncheckpointed SQLite WAL")
	}
}

func TestSQLiteCutoverPreflightRequiresVerifiedBackup(t *testing.T) {
	root, _, _, _, _ := sqliteReindexFixture(t)
	if err := VerifySQLiteCutoverPreflight(root, ""); err == nil {
		t.Fatal("cutover preflight accepted missing rollback snapshot")
	}
	if err := VerifySQLiteCutoverPreflight(root, filepath.Join(t.TempDir(), "nonexistent")); err == nil {
		t.Fatal("cutover preflight accepted nonexistent recovery point")
	}
}

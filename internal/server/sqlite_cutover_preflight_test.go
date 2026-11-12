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

func TestSQLiteCutoverPreflightRejectsNewOptionalMetadata(t *testing.T) {
	root, backup, _, _, _ := sqliteReindexFixture(t)
	for _, name := range []string{"accounts.json", "authors.json"} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := VerifySQLiteCutoverPreflight(root, backup); err == nil {
			t.Fatalf("preflight accepted %s created after recovery snapshot", name)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSQLiteCutoverPreflightRejectsModifiedOrMissingOptionalMetadata(t *testing.T) {
	root, _, _, _ := sqliteSnapshotAuditFixture(t)
	originals := map[string][]byte{
		"accounts.json": []byte(`{"accounts":{}}`),
		"authors.json":  []byte(`{"first":["creator"]}`),
	}
	for name, raw := range originals {
		if err := os.WriteFile(filepath.Join(root, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	backup := filepath.Join(t.TempDir(), "recovery")
	if err := PrepareSQLiteBackup(root, backup); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteCutoverPreflight(root, backup); err != nil {
		t.Fatalf("fresh backup with optional metadata rejected: %v", err)
	}
	for name, original := range originals {
		path := filepath.Join(root, name)
		t.Run(name+"/modified", func(t *testing.T) {
			if err := os.WriteFile(path, []byte(`{"changed":true}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := VerifySQLiteCutoverPreflight(root, backup); err == nil {
				t.Fatal("preflight accepted changed account/author metadata")
			}
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
		})
		t.Run(name+"/deleted", func(t *testing.T) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := VerifySQLiteCutoverPreflight(root, backup); err == nil {
				t.Fatal("preflight accepted deleted account/author metadata")
			}
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
		})
	}
}

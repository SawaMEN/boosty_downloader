package server

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteShadowRestoreRepairsCorruptDatabaseWithoutChangingJSON(t *testing.T) {
	root, backup, _, _, _ := sqliteReindexFixture(t)
	originalJobs, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	originalImports, err := os.ReadFile(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile(filepath.Join(backup, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(root, "metadata.db")
	if err := os.WriteFile(db, []byte("damaged offline SQL"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSQLiteShadowFromBackup(root, backup); err != nil {
		t.Fatalf("valid detached shadow restore failed: %v", err)
	}
	actual, err := os.ReadFile(db)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatal("restored SQLite is not byte-identical to the verified backup")
	}
	for _, test := range []struct {
		name string
		want []byte
	}{
		{"jobs.json", originalJobs},
		{"imports.json", originalImports},
	} {
		got, err := os.ReadFile(filepath.Join(root, test.name))
		if err != nil || !bytes.Equal(got, test.want) {
			t.Fatalf("restore changed authoritative %s: %v", test.name, err)
		}
	}
	if err := VerifySQLiteCutoverPreflight(root, backup); err != nil {
		t.Fatalf("restored SQLite failed full offline inventory: %v", err)
	}
}

func TestSQLiteShadowRestoreRecreatesMissingIndex(t *testing.T) {
	root, backup, _, _, _ := sqliteReindexFixture(t)
	if err := os.Remove(filepath.Join(root, "metadata.db")); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSQLiteShadowFromBackup(root, backup); err != nil {
		t.Fatalf("missing detached SQL index should be recoverable: %v", err)
	}
	if err := VerifySQLiteExport(root); err != nil {
		t.Fatalf("recreated SQLite did not match JSON: %v", err)
	}
}

func TestSQLiteShadowRestoreRejectsStaleJSONOrChangedMedia(t *testing.T) {
	t.Run("JSON ACL", func(t *testing.T) {
		root, backup, _, _, _ := sqliteReindexFixture(t)
		db := filepath.Join(root, "metadata.db")
		oldSQL, err := os.ReadFile(db)
		if err != nil {
			t.Fatal(err)
		}
		jobFile := filepath.Join(root, "jobs.json")
		raw, err := os.ReadFile(jobFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(jobFile, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		if err := RestoreSQLiteShadowFromBackup(root, backup); err == nil {
			t.Fatal("restore accepted changed authoritative JSON")
		}
		after, err := os.ReadFile(db)
		if err != nil || !bytes.Equal(after, oldSQL) {
			t.Fatal("rejected restore changed SQLite")
		}
	})
	t.Run("media", func(t *testing.T) {
		root, backup, _, history, media := sqliteReindexFixture(t)
		db := filepath.Join(root, "metadata.db")
		oldSQL, err := os.ReadFile(db)
		if err != nil {
			t.Fatal(err)
		}
		mediaFile := filepath.Join(root, "revisions", "first", "creator", "post-1", history, "media", media)
		if err := os.WriteFile(mediaFile, []byte("corrupt media"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := RestoreSQLiteShadowFromBackup(root, backup); err == nil {
			t.Fatal("restore ignored changed historical media")
		}
		after, err := os.ReadFile(db)
		if err != nil || !bytes.Equal(after, oldSQL) {
			t.Fatal("media mismatch caused SQLite mutation")
		}
	})
	t.Run("new history", func(t *testing.T) {
		root, backup, _, history, media := sqliteReindexFixture(t)
		sqliteReindexCopyHistory(t, root, history, strings.Repeat("c", 24), media)
		if err := RestoreSQLiteShadowFromBackup(root, backup); err == nil {
			t.Fatal("restore accepted new archive history absent from backup")
		}
	})
	t.Run("new account file", func(t *testing.T) {
		root, backup, _, _, _ := sqliteReindexFixture(t)
		if err := os.WriteFile(filepath.Join(root, "accounts.json"), []byte(`{"accounts":{}}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := RestoreSQLiteShadowFromBackup(root, backup); err == nil {
			t.Fatal("restore accepted post-backup account state change")
		}
	})
}

func TestSQLiteShadowRestoreRejectsBusyOrUnsafeDatabase(t *testing.T) {
	t.Run("busy", func(t *testing.T) {
		root, backup, _, _, _ := sqliteReindexFixture(t)
		lock, err := lockDataDirectory(root)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := RestoreSQLiteShadowFromBackup(root, backup); err == nil {
			t.Fatal("restore accepted directory locked by another process")
		}
	})
	t.Run("WAL", func(t *testing.T) {
		root, backup, _, _, _ := sqliteReindexFixture(t)
		if err := os.WriteFile(filepath.Join(root, "metadata.db-wal"), []byte("uncheckpointed"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := RestoreSQLiteShadowFromBackup(root, backup); err == nil {
			t.Fatal("restore ignored an uncheckpointed SQLite WAL")
		}
	})
	t.Run("journal", func(t *testing.T) {
		root, backup, _, _, _ := sqliteReindexFixture(t)
		if err := os.WriteFile(filepath.Join(root, metadataJournalName), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := RestoreSQLiteShadowFromBackup(root, backup); err == nil {
			t.Fatal("restore ignored incomplete JSON transaction")
		}
	})
	t.Run("unsafe shared-memory sidecar", func(t *testing.T) {
		root, backup, _, _, _ := sqliteReindexFixture(t)
		shm := filepath.Join(root, "metadata.db-shm")
		if err := os.Remove(shm); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.Symlink("jobs.json", shm); err != nil {
			t.Fatal(err)
		}
		if err := RestoreSQLiteShadowFromBackup(root, backup); err == nil {
			t.Fatal("restore accepted symlinked SQLite shared-memory sidecar")
		}
	})
	t.Run("rollback journal", func(t *testing.T) {
		root, backup, _, _, _ := sqliteReindexFixture(t)
		if err := os.WriteFile(filepath.Join(root, "metadata.db-journal"), []byte("pending"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := RestoreSQLiteShadowFromBackup(root, backup); err == nil {
			t.Fatal("restore accepted leftover SQLite rollback journal")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		root, backup, _, _, _ := sqliteReindexFixture(t)
		db := filepath.Join(root, "metadata.db")
		if err := os.Remove(db); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("jobs.json", db); err != nil {
			t.Fatal(err)
		}
		if err := RestoreSQLiteShadowFromBackup(root, backup); err == nil {
			t.Fatal("restore followed symbolic link for SQLite destination")
		}
	})
	t.Run("backup tamper", func(t *testing.T) {
		root, backup, _, _, _ := sqliteReindexFixture(t)
		f := filepath.Join(backup, "metadata.db")
		contents, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, append(contents, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
		if err := RestoreSQLiteShadowFromBackup(root, backup); err == nil {
			t.Fatal("restore accepted corrupted backup")
		}
	})
}

package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDataDirLockExclusiveAndReleasedOnClose(t *testing.T) {
	root := t.TempDir()
	first, err := lockDataDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockDataDirectory(root); err == nil {
		second.Close()
		first.Close()
		t.Fatal("another lock on the same directory was allowed")
	} else if !strings.Contains(err.Error(), "locked") {
		first.Close()
		t.Fatalf("unexpected concurrent lock failure: %v", err)
	}
	other, err := lockDataDirectory(t.TempDir())
	if err != nil {
		t.Fatal("unrelated data directory should remain available:", err)
	}
	other.Close()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := lockDataDirectory(root)
	if err != nil {
		t.Fatal("directory lock was not released on close:", err)
	}
	next.Close()
}

func TestDataDirLockRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if held, err := lockDataDirectory(link); err == nil {
		held.Close()
		t.Fatal("symlink data root was accepted")
	}
	file := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if held, err := lockDataDirectory(file); err == nil {
		held.Close()
		t.Fatal("regular file data root was accepted")
	}
	if held, err := lockDataDirectory(""); err == nil {
		held.Close()
		t.Fatal("empty data root was accepted")
	}
}

func TestRunningAppBlocksSecondServerAndOfflineSQLiteOperations(t *testing.T) {
	root, backup, _, _, _ := sqliteReindexFixture(t)
	cfg := Config{DataDir: root, SecretKey: testKey(t),
		AdminUser: "admin", AdminPassword: "strong-password-123"}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	second, err := New(cfg)
	if err == nil {
		second.Close()
		t.Fatal("second server started on live data directory")
	}
	checks := []struct {
		name string
		run func() error
	}{
		{"backup", func() error {
			return PrepareSQLiteBackup(root, filepath.Join(t.TempDir(), "backup"))
		}},
		{"cutover", func() error { return VerifySQLiteCutoverPreflight(root, backup) }},
		{"reindex", func() error { return ReindexSQLiteRevisionInventory(root, backup) }},
		{"export", func() error { return VerifySQLiteExport(root) }},
		{"media-audit", func() error { return VerifySQLiteSnapshotInventory(root) }},
		{"password-reset", func() error {
			return ResetAdminPassword(root, "admin", "strong-password-123", "new-long-password-123")
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			err := check.run()
			if err == nil || !strings.Contains(err.Error(), "locked") {
				t.Fatalf("live directory operation must fail on lock, got %v", err)
			}
		})
	}
	app.Close()
	if err := VerifySQLiteExport(root); err != nil {
		t.Fatalf("offline SQLite export check did not resume after shutdown: %v", err)
	}
	if lock, err := lockDataDirectory(root); err != nil {
		t.Fatalf("directory remained locked after server close: %v", err)
	} else {
		lock.Close()
	}
}

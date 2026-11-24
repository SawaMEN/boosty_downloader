package server

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteRecoveryBundlePreparesWithoutActivating(t *testing.T) {
	cfg, _, _ := sqlActivationFixture(t)
	root := cfg.DataDir
	dest := filepath.Join(t.TempDir(), "bundle")
	before, err := os.ReadFile(filepath.Join(root, sqliteBackendSelectionName))
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareSQLiteRecoveryBundle(root, dest); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, sqliteBackendSelectionName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("bundle changed backend selection", err)
	}
	backup, full := filepath.Join(dest, "metadata"), filepath.Join(dest, "full")
	if err := VerifySQLiteCutoverFullPreflight(root, backup, full); err != nil {
		t.Fatal(err)
	}
	if err := PrepareSQLiteRecoveryBundle(root, dest); err == nil {
		t.Fatal("existing bundle overwritten")
	}
	if err := VerifyFullDataSnapshot(full); err != nil {
		t.Fatal("existing full snapshot changed", err)
	}
	if err := os.WriteFile(filepath.Join(root, "later-private-file"), []byte("later"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteCutoverFullPreflight(root, backup, full); err == nil {
		t.Fatal("stale bundle accepted")
	}
}

func TestSQLiteRecoveryBundleRejectsBusyAndOverlappingSource(t *testing.T) {
	cfg, _, _ := sqlActivationFixture(t)
	root := cfg.DataDir
	inside := filepath.Join(root, "bundle")
	if err := PrepareSQLiteRecoveryBundle(root, inside); err == nil {
		t.Fatal("bundle inside live source accepted")
	}
	if _, err := os.Lstat(inside); !os.IsNotExist(err) {
		t.Fatal("overlap created destination", err)
	}
	dest := filepath.Join(t.TempDir(), "bundle")
	lock, err := lockDataDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := PrepareSQLiteRecoveryBundle(root, dest); err == nil {
		t.Fatal("busy source accepted")
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatal("busy source created destination", err)
	}
}

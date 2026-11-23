package server

import (
	"os"
	"path/filepath"
	"testing"
)

func fullCutoverFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	root, backup, _, _, _ := sqliteReindexFixture(t)
	secret := filepath.Join(root, "private-token-fixture")
	if err := os.WriteFile(secret, []byte("synthetic token A"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(t.TempDir(), "full-recovery")
	if err := PrepareFullDataSnapshot(root, snapshot); err != nil {
		t.Fatal(err)
	}
	return root, backup, snapshot, secret
}

func TestSQLiteCutoverFullSnapshotPreflight(t *testing.T) {
	root, backup, snapshot, secret := fullCutoverFixture(t)
	if err := VerifySQLiteCutoverFullPreflight(root, backup, snapshot); err != nil {
		t.Fatal(err)
	}
	// Same-size edits to files outside the SQLite inventory must be found.
	if err := os.WriteFile(secret, []byte("synthetic token B"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteCutoverFullPreflight(root, backup, snapshot); err == nil {
		t.Fatal("cutover accepted stale full snapshot after token change")
	}
	if err := os.WriteFile(secret, []byte("synthetic token A"), 0600); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(root, "untracked-private-file")
	if err := os.WriteFile(extra, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteCutoverFullPreflight(root, backup, snapshot); err == nil {
		t.Fatal("cutover accepted missing recovery file")
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0644); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteCutoverFullPreflight(root, backup, snapshot); err == nil {
		t.Fatal("cutover accepted changed source permissions")
	}
}

func TestSQLiteCutoverFullSnapshotRecoveryRejectsChanges(t *testing.T) {
	root, backup, snapshot, secret := fullCutoverFixture(t)
	if err := PrepareSQLiteCutoverFullRehearsal(root, backup, snapshot); err != nil {
		t.Fatal(err)
	}
	intent, found, err := readSQLiteCutoverIntent(root)
	if err != nil || !found || intent.Version != 2 || intent.FullSnapshotHash == "" {
		t.Fatalf("missing full cutover recovery binding: %v %+v", err, intent)
	}
	if err := os.WriteFile(secret, []byte("synthetic token B"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverSQLiteCutoverRehearsal(root); err == nil {
		t.Fatal("rehearsal recovery accepted newer token bytes")
	}
	if _, err := os.Lstat(filepath.Join(root, sqliteCutoverIntentName)); err != nil {
		t.Fatal("failed verification deleted intent")
	}
	if err := os.WriteFile(secret, []byte("synthetic token A"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(snapshot, fullSnapshotManifestName)
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, append(append([]byte(nil), raw...), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverSQLiteCutoverRehearsal(root); err == nil {
		t.Fatal("recovery accepted altered pinned full manifest")
	}
	if err := os.WriteFile(manifest, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverSQLiteCutoverRehearsal(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, sqliteCutoverIntentName)); !os.IsNotExist(err) {
		t.Fatal("successful recovery left intent")
	}
	if mode, err := readSQLiteBackendSelection(root); err != nil || mode != "json" {
		t.Fatal("rehearsal changed backend", mode, err)
	}
}

func TestSQLiteCutoverFullSnapshotRejectsPendingRollbackJournal(t *testing.T) {
	root, backup, snapshot, _ := fullCutoverFixture(t)
	if err := os.WriteFile(filepath.Join(root, "metadata.db-journal"), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteCutoverFullPreflight(root, backup, snapshot); err == nil {
		t.Fatal("cutover accepted pending SQLite rollback journal")
	}
}

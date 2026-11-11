package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSQLiteCutoverBackupCreatesPrivateVerifiedMetadata(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	if err := os.WriteFile(filepath.Join(root, "authors.json"), []byte(`{"first":["creator"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "accounts.json"), []byte(`{"accounts":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "snapshot")
	if err := PrepareSQLiteBackup(root, dest); err != nil {
		t.Fatal(err)
	}
	stat, err := os.Stat(dest)
	if err != nil || stat.Mode().Perm() != 0700 {
		t.Fatalf("backup directory is not private: %v %v", stat, err)
	}
	raw, err := os.ReadFile(filepath.Join(dest, "backup-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest metadataBackupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != 1 || len(manifest.Files) != 5 {
		t.Fatalf("backup did not include all five metadata files: %+v", manifest)
	}
	for _, file := range manifest.Files {
		backup := filepath.Join(dest, file.Name)
		copyBytes, err := os.ReadFile(backup)
		if err != nil {
			t.Fatal(err)
		}
		sourceBytes, err := os.ReadFile(filepath.Join(root, file.Name))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(copyBytes)
		if file.Size != int64(len(copyBytes)) || file.SHA256 != hex.EncodeToString(digest[:]) ||
			string(copyBytes) != string(sourceBytes) {
			t.Fatalf("backup checksum differs from source: %s", file.Name)
		}
		info, err := os.Stat(backup)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("backup file must be private: %s %v", file.Name, err)
		}
	}
	if err := PrepareSQLiteBackup(root, dest); err == nil {
		t.Fatal("existing backup destination was overwritten")
	}
}

func TestSQLiteCutoverBackupRejectsStaleExport(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	// An ACL change in authoritative JSON must refuse a seemingly valid SQL
	// snapshot; no partial recovery directory should be published.
	file := filepath.Join(root, "jobs.json")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var jobs map[string]Job
	if err := json.Unmarshal(raw, &jobs); err != nil {
		t.Fatal(err)
	}
	for id, j := range jobs {
		j.SharedWith = []string{"unapproved"}
		jobs[id] = j
		break
	}
	changed, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, changed, 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "stale")
	if err := PrepareSQLiteBackup(root, dest); err == nil {
		t.Fatal("stale SQLite snapshot accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("failed backup unexpectedly created destination: %v", err)
	}
}

func TestSQLiteCutoverBackupRefusesUnsafeTargetsAndUncheckpointedWAL(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	if err := PrepareSQLiteBackup(root, filepath.Join(root, "backups", "bad")); err == nil {
		t.Fatal("backup allowed inside live data directory")
	}
	dest := filepath.Join(t.TempDir(), "metadata")
	if err := os.Symlink(root, dest); err != nil {
		t.Fatal(err)
	}
	if err := PrepareSQLiteBackup(root, dest); err == nil {
		t.Fatal("symlink/occupied backup destination accepted")
	}
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "metadata.db-wal"), []byte("dirty-WAL"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareSQLiteBackup(root, dest); err == nil {
		t.Fatal("uncheckpointed WAL accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("WAL error left a partial backup: %v", err)
	}
}

func TestSQLiteCutoverBackupRefusesPendingJournalAndMetadataSymlinks(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	dest := filepath.Join(t.TempDir(), "snapshot")
	if err := os.WriteFile(filepath.Join(root, metadataJournalName), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareSQLiteBackup(root, dest); err == nil {
		t.Fatal("pending metadata journal ignored")
	}
	if err := os.Remove(filepath.Join(root, metadataJournalName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("jobs.json", filepath.Join(root, "authors.json")); err != nil {
		t.Fatal(err)
	}
	if err := PrepareSQLiteBackup(root, dest); err == nil {
		t.Fatal("metadata symlink accepted")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("unsafe backup was left on disk: %v", err)
	}
}

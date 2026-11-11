package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func metadataBackupVerificationFixture(t *testing.T) string {
	t.Helper()
	root, _ := sqliteCanaryFixture(t)
	dest := filepath.Join(t.TempDir(), "recovery")
	if err := PrepareSQLiteBackup(root, dest); err != nil {
		t.Fatal(err)
	}
	return dest
}

func TestSQLiteMetadataBackupVerifierAcceptsUntouchedSnapshot(t *testing.T) {
	dest := metadataBackupVerificationFixture(t)
	if err := VerifySQLiteMetadataBackup(dest); err != nil {
		t.Fatal("valid recovery snapshot rejected:", err)
	}
}

func TestSQLiteMetadataBackupVerifierRejectsAlteredContent(t *testing.T) {
	dest := metadataBackupVerificationFixture(t)
	path := filepath.Join(dest, "imports.json")
	file, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(file, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteMetadataBackup(dest); err == nil {
		t.Fatal("checksum mismatch in import backup was ignored")
	}
}

func TestSQLiteMetadataBackupVerifierRejectsMissingOrSymlinkedFiles(t *testing.T) {
	dest := metadataBackupVerificationFixture(t)
	if err := os.Remove(filepath.Join(dest, "imports.json")); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteMetadataBackup(dest); err == nil {
		t.Fatal("backup with missing imports.json accepted")
	}
	dest = metadataBackupVerificationFixture(t)
	if err := os.Rename(filepath.Join(dest, "jobs.json"), filepath.Join(dest, "jobs.saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("jobs.saved", filepath.Join(dest, "jobs.json")); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteMetadataBackup(dest); err == nil {
		t.Fatal("symlinked backup metadata accepted")
	}
}

func TestSQLiteMetadataBackupVerifierRejectsCorruptSQLEvenWithValidChecksum(t *testing.T) {
	dest := metadataBackupVerificationFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(dest, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE owner_archives SET shared_for_all=1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	manifestFile := filepath.Join(dest, "backup-manifest.json")
	b, err := os.ReadFile(manifestFile)
	if err != nil {
		t.Fatal(err)
	}
	var manifest metadataBackupManifest
	if err := json.Unmarshal(b, &manifest); err != nil {
		t.Fatal(err)
	}
	dbBytes, err := os.ReadFile(filepath.Join(dest, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(dbBytes)
	for i := range manifest.Files {
		if manifest.Files[i].Name == "metadata.db" {
			manifest.Files[i].SHA256 = hex.EncodeToString(hash[:])
			manifest.Files[i].Size = int64(len(dbBytes))
		}
	}
	b, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestFile, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteMetadataBackup(dest); err == nil {
		t.Fatal("backup accepted forged SQLite access flags with recomputed checksums")
	}
}

func TestSQLiteMetadataBackupVerifierRejectsUnknownFilesOrManifests(t *testing.T) {
	dest := metadataBackupVerificationFixture(t)
	if err := os.WriteFile(filepath.Join(dest, "hidden-token.enc"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteMetadataBackup(dest); err == nil {
		t.Fatal("unexpected backup file accepted")
	}
	dest = metadataBackupVerificationFixture(t)
	path := filepath.Join(dest, "backup-manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest metadataBackupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Files = append(manifest.Files, manifest.Files[0])
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteMetadataBackup(dest); err == nil {
		t.Fatal("duplicate backup manifest entries accepted")
	}
}

func TestSQLiteMetadataBackupVerifierRejectsWorldReadableSnapshot(t *testing.T) {
	dest := metadataBackupVerificationFixture(t)
	if err := os.Chmod(filepath.Join(dest, "jobs.json"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteMetadataBackup(dest); err == nil {
		t.Fatal("world-readable archive metadata backup accepted")
	}
	if err := os.Chmod(filepath.Join(dest, "jobs.json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dest, 0755); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteMetadataBackup(dest); err == nil {
		t.Fatal("world-readable backup directory accepted")
	}
}

func TestSQLiteMetadataBackupVerifierRejectsInvalidJSONWithNewChecksum(t *testing.T) {
	dest := metadataBackupVerificationFixture(t)
	path := filepath.Join(dest, "imports.json")
	if err := os.WriteFile(path, []byte(strings.Repeat("{", 10)), 0600); err != nil {
		t.Fatal(err)
	}
	manifestFile := filepath.Join(dest, "backup-manifest.json")
	data, err := os.ReadFile(manifestFile)
	if err != nil {
		t.Fatal(err)
	}
	var manifest metadataBackupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	newBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range manifest.Files {
		if manifest.Files[i].Name == "imports.json" {
			manifest.Files[i].SHA256 = metadataDigest(newBytes)
			manifest.Files[i].Size = int64(len(newBytes))
		}
	}
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestFile, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteMetadataBackup(dest); err == nil {
		t.Fatal("invalid JSON accepted after checksum recomputation")
	}
}

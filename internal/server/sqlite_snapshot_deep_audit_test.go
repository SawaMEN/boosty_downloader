package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteDeepAuditRejectsUnindexedOwnerSnapshots(t *testing.T) {
	root, _, _, _ := sqliteSnapshotAuditFixture(t)
	orphan := filepath.Join(root, "library", "other", "creator", "orphan-post")
	if err := os.MkdirAll(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	// The archived copy belongs to a different owner and is absent from
	// SQLite, even though every SQL-indexed revision is still valid.
	if err := os.WriteFile(filepath.Join(orphan, "manifest.json"),
		[]byte(`{"author":"creator","postId":"orphan-post"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("unindexed owner archive was not detected")
	}
}

func TestSQLiteDeepAuditRejectsUnindexedHistoricalSnapshot(t *testing.T) {
	root, _, _, _ := sqliteSnapshotAuditFixture(t)
	orphan := filepath.Join(root, "revisions", "first", "creator", "post-1", strings.Repeat("c", 24))
	if err := os.MkdirAll(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "manifest.json"),
		[]byte(`{"author":"creator","postId":"post-1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("unindexed historical snapshot was not detected")
	}
}

func TestSQLiteDeepAuditChecksCanonicalCASContents(t *testing.T) {
	root, _, _, filename := sqliteSnapshotAuditFixture(t)
	media, err := os.ReadFile(filepath.Join(root, "library", "first", "creator", "post-1", "media", filename))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(media)
	digest := hex.EncodeToString(sum[:])
	dir := filepath.Join(root, "blobs", "sha256", digest[:2])
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cas := filepath.Join(dir, digest)
	if err := os.WriteFile(cas, media, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	canonical := filepath.ToSlash(filepath.Join("blobs", "sha256", digest[:2], digest))
	if _, err := db.Exec("UPDATE media_blobs SET relative_path=? WHERE sha256=?", canonical, digest); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err != nil {
		t.Fatalf("valid CAS rejected: %v", err)
	}
	if err := os.WriteFile(cas, []byte(strings.Repeat("x", len(media))), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("corrupted CAS with same byte length passed audit")
	}
}

func TestSQLiteDeepAuditRejectsNonCanonicalCASAndSymlink(t *testing.T) {
	root, _, _, filename := sqliteSnapshotAuditFixture(t)
	media, err := os.ReadFile(filepath.Join(root, "library", "first", "creator", "post-1", "media", filename))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(media)
	digest := hex.EncodeToString(sum[:])
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE media_blobs SET relative_path='blobs/sha256/other' WHERE sha256=?", digest); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("noncanonical SQL CAS path accepted")
	}
	canonical := filepath.ToSlash(filepath.Join("blobs", "sha256", digest[:2], digest))
	db, err = sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE media_blobs SET relative_path=? WHERE sha256=?", canonical, digest); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "blobs", "sha256", digest[:2])
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "library", "first", "creator", "post-1", "media", filename), filepath.Join(dir, digest)); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("SQL CAS target symlink accepted")
	}
}

func TestSQLiteDeepAuditRejectsOrphanForeignKeys(t *testing.T) {
	root, _, _, _ := sqliteSnapshotAuditFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO archive_grants
		(archive_id,grantee_owner_id,created_at)
		VALUES ('missing-owner','someone',?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("SQL orphan grant was ignored by audit joins")
	}
}

func TestSQLiteDeepAuditAllowsIncompleteRevisionStage(t *testing.T) {
	root, _, _, _ := sqliteSnapshotAuditFixture(t)
	dir := filepath.Join(root, "revisions", "first", "creator", "post-1", ".incomplete-"+strings.Repeat("d", 24))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err != nil {
		t.Fatalf("uncommitted staging directory blocked offline audit: %v", err)
	}
}

func TestSQLiteDeepAuditRejectsUncheckpointedWAL(t *testing.T) {
	root, _, _, _ := sqliteSnapshotAuditFixture(t)
	wal := filepath.Join(root, "metadata.db-wal")
	if err := os.WriteFile(wal, []byte("pending-SQL-writes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("uncheckpointed SQLite WAL must not be silently ignored")
	}
}

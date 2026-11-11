package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sqliteSnapshotAuditFixture(t *testing.T) (root, jobID, histID, media string) {
	t.Helper()
	root, jobs := sqliteCanaryFixture(t)
	jobID = strings.Repeat("1", 24)
	histID = strings.Repeat("a", 24)
	media = "clip.mp4"
	job := jobs[jobID]
	contents := []byte("immutable media from first owner's archive")
	digest := sha256.Sum256(contents)
	man := ArchiveManifest{
		Author: job.Author, PostID: job.PostID, Title: "Archived fixture",
		CapturedAt: job.UpdatedAt,
		Assets: []ArchiveAsset{{
			Filename: media, Type: "video", Size: int64(len(contents)),
			SHA256: hex.EncodeToString(digest[:]),
		}},
	}
	raw, err := json.Marshal(man)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{
		filepath.Join(root, "library", job.OwnerID, job.Author, job.PostID),
		filepath.Join(root, "revisions", job.OwnerID, job.Author, job.PostID, histID),
	} {
		if err := os.MkdirAll(filepath.Join(dir, "media"), 0700); err != nil {
			t.Fatal(err)
		}
		for name, data := range map[string][]byte{
			"index.html": []byte("<html>private</html>"),
			"manifest.json": raw,
			filepath.Join("media", media): contents,
		} {
			if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Remove(filepath.Join(root, "metadata.db")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("python3", "../../tools/sqlite_migrate.py",
		"--data-dir", root, "--output", filepath.Join(root, "metadata.db"))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture export: %v\n%s", err, output)
	}
	return root, jobID, histID, media
}

func TestSQLiteSnapshotAuditValidatesHistoryAndMedia(t *testing.T) {
	root, jobID, histID, _ := sqliteSnapshotAuditFixture(t)
	if err := VerifySQLiteSnapshotInventory(root); err != nil {
		t.Fatalf("valid historical inventory rejected: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var snapshots, assets int
	if err := db.QueryRow("SELECT COUNT(*) FROM archive_revisions WHERE archive_id=?", jobID).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM revision_assets").Scan(&assets); err != nil {
		t.Fatal(err)
	}
	if snapshots != 2 || assets != 2 {
		t.Fatalf("expected current and historical media snapshots; snapshots=%d assets=%d hist=%s", snapshots, assets, histID)
	}
}

func TestSQLiteSnapshotAuditRejectsOrphanHistoricalFiles(t *testing.T) {
	root, _, histID, _ := sqliteSnapshotAuditFixture(t)
	original := filepath.Join(root, "revisions", "first", "creator", "post-1", histID)
	orphan := filepath.Join(root, "revisions", "first", "creator", "post-1", strings.Repeat("b", 24))
	if err := os.Rename(original, orphan); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("revision moved to an unindexed directory passed audit")
	}
}

func TestSQLiteSnapshotAuditRejectsMediaTamperingAndMissingMedia(t *testing.T) {
	root, _, _, media := sqliteSnapshotAuditFixture(t)
	file := filepath.Join(root, "library", "first", "creator", "post-1", "media", media)
	if err := os.WriteFile(file, []byte(strings.Repeat("x", 49)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("media bytes changed but audit passed")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("missing media file passed audit")
	}
}

func TestSQLiteSnapshotAuditRejectsWrongOwnerAndSymlink(t *testing.T) {
	root, jobID, _, media := sqliteSnapshotAuditFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE archive_revisions SET snapshot_relpath=? WHERE revision_id=?",
		"library/second/creator/post-1", "current:"+jobID); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("cross-owner revision path accepted")
	}
	root, _, _, media = sqliteSnapshotAuditFixture(t)
	file := filepath.Join(root, "library", "first", "creator", "post-1", "media", media)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "revisions", "first", "creator", "post-1",
		strings.Repeat("a", 24), "media", media), file); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("symlink into another snapshot accepted")
	}
}

func TestSQLiteSnapshotAuditRejectsSQLAssetTamper(t *testing.T) {
	root, _, _, _ := sqliteSnapshotAuditFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE media_blobs SET byte_length=byte_length+1")
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("SQL media size tampering was not detected")
	}
}

func TestSQLiteSnapshotAuditReadOnly(t *testing.T) {
	root, _, _, _ := sqliteSnapshotAuditFixture(t)
	before, err := os.Stat(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("read-only audit modified SQLite source")
	}
}

func TestSQLiteSnapshotAuditRejectsUnexpectedVersionEntries(t *testing.T) {
	root, _, _, _ := sqliteSnapshotAuditFixture(t)
	dir := filepath.Join(root, "revisions", "first", "creator", "post-1", "unexpected-file")
	if err := os.WriteFile(dir, []byte("not a revision"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("unexpected revision entry accepted")
	}
}

func TestSQLiteSnapshotAuditRejectsArchivedDateMismatch(t *testing.T) {
	root, jobID, _, _ := sqliteSnapshotAuditFixture(t)
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("UPDATE archive_revisions SET captured_at=? WHERE revision_id=?",
		time.Date(2020, 1, 1, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"current:"+jobID)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifySQLiteSnapshotInventory(root); err == nil {
		t.Fatal("historical capture date index tampering accepted")
	}
}

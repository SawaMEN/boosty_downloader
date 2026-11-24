package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Run the actual inventory writer in an uncommitted transaction, then
// kill the process without defers. The parent observes SQLite recovery.
func TestSQLiteMediaShadowLargeCrashWorker(t *testing.T) {
	root := os.Getenv("BOOSTY_TEST_MEDIA_CRASH_ROOT")
	if root == "" {
		return
	}
	ctx := context.Background()
	lock, err := lockDataDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	jobs, err := sqliteShadowReadJobs(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	revisions, err := sqliteReindexDiskContext(ctx, root, jobs)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.close()
	tx, err := meta.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := sqliteApplyRevisionInventory(ctx, tx, revisions, jobs, imports); err != nil {
		t.Fatal(err)
	}
	var size int64
	if err := tx.QueryRow("SELECT MAX(byte_length) FROM media_blobs").Scan(&size); err != nil || size < 16<<20 {
		t.Fatalf("new media not staged: %d %v", size, err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	t.Fatal("crash worker returned")
}

func TestSQLiteMediaShadowLargeCrashRetry(t *testing.T) {
	root, jobID, historyID, media := sqliteSnapshotAuditFixture(t)
	dir := filepath.Join(root, "library", "first", "creator", "post-1")
	path := filepath.Join(dir, "media", media)
	size := int64(16 << 20)
	if os.Getenv("BOOSTY_TEST_LARGE_MEDIA") == "1" {
		size = (2 << 30) + 17
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xa5}, size-1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	digest, actual, err := sqliteReindexHashFile(path)
	if err != nil || actual != size {
		t.Fatalf("large fixture: %d %v", actual, err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest ArchiveManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Assets[0].Size, manifest.Assets[0].SHA256 = size, digest
	if err := writeJSON(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{}
	for _, name := range []string{"jobs.json", "imports.json"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = data
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSQLiteMediaShadowLargeCrashWorker$")
	cmd.Env = append(os.Environ(), "BOOSTY_TEST_MEDIA_CRASH_ROOT="+root)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || ctx.Err() != nil {
		t.Fatalf("worker failed to crash: %v %s", err, out)
	}
	ws, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("unexpected worker exit: %v %s", err, out)
	}
	lock, err := lockDataDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.close()
	readAsset := func(revision string) (string, int64) {
		t.Helper()
		var sha string
		var n int64
		if err := meta.db.QueryRow(`SELECT a.sha256,b.byte_length FROM revision_assets a
			JOIN media_blobs b ON a.sha256=b.sha256 WHERE a.revision_id=?`, revision).Scan(&sha, &n); err != nil {
			t.Fatal(err)
		}
		return sha, n
	}
	oldSHA, oldSize := readAsset(historyID)
	if sha, n := readAsset("current:" + jobID); sha != oldSHA || n != oldSize || n == size {
		t.Fatal("uncommitted index survived crash")
	}
	store := &Storage{root: root, shadow: &sqliteShadow{meta: meta, wake: make(chan struct{}, 1), mediaEnabled: true, mediaChanged: 1}}
	if err := store.syncSQLiteShadow(ctx); err != nil {
		t.Fatal("retry", err)
	}
	if sha, n := readAsset("current:" + jobID); sha != digest || n != size {
		t.Fatal("large media index differs after retry")
	}
	if sha, n := readAsset(historyID); sha != oldSHA || n != oldSize {
		t.Fatal("history changed")
	}
	if store.sqliteShadowStatus().MediaPending {
		t.Fatal("retry did not clear pending event")
	}
	f, err = os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0x5a}, size-1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	store.notifySQLiteMedia()
	if err := store.syncSQLiteShadow(ctx); err == nil {
		t.Fatal("damaged large file accepted")
	}
	if sha, n := readAsset("current:" + jobID); sha != digest || n != size {
		t.Fatal("failed audit replaced committed index")
	}
	if !store.sqliteShadowStatus().MediaPending {
		t.Fatal("failed media audit discarded pending event")
	}
	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || !bytes.Equal(want, got) {
			t.Fatalf("authoritative JSON changed: %s %v", name, err)
		}
	}
	t.Logf("verified crash rollback and retry for %d logical media bytes", size)
}

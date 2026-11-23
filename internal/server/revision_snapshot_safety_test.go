package server

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// Media symlinks must not let a history snapshot reference another archive.
func TestArchiveSnapshotRejectsSymlinkedMediaParent(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{live, outside} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	manifest := ArchiveManifest{
		Author: "creator", PostID: "post-a",
		Assets: []ArchiveAsset{{Filename: "movie.mp4", Type: "ok_video"}},
	}
	if err := writeJSON(filepath.Join(live, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "index.html"), []byte("local html"), 0600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(outside, "movie.mp4")
	if err := os.WriteFile(foreign, []byte("other user's media"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(live, "media")); err != nil {
		t.Fatal(err)
	}
	if _, err := linkArchiveSnapshot(live, filepath.Join(root, "bad-revision")); err == nil {
		t.Fatal("snapshot accepted media through a symlinked parent")
	}
	if _, err := os.Lstat(filepath.Join(root, "bad-revision", "media", "movie.mp4")); !os.IsNotExist(err) {
		t.Fatalf("foreign media was linked into the snapshot: %v", err)
	}
	if err := os.Remove(filepath.Join(live, "media")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(live, "media"), 0700); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(live, "media", "movie.mp4")
	if err := os.WriteFile(local, []byte("local media"), 0600); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(root, "good-revision")
	if _, err := linkArchiveSnapshot(live, good); err != nil {
		t.Fatalf("normal archive snapshot failed: %v", err)
	}
	originalInfo, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}
	snapshotInfo, err := os.Stat(filepath.Join(good, "media", "movie.mp4"))
	if err != nil || !os.SameFile(originalInfo, snapshotInfo) {
		t.Fatalf("snapshot did not preserve pinned media inode: %v", err)
	}
}

func TestRestoreManifestDigestRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	if err := os.Mkdir(live, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "private.json")
	if err := os.WriteFile(outside, []byte(`{"author":"creator","postId":"post-a"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(live, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := revisionRestoreManifestDigest(live, "creator", "post-a"); err == nil {
		t.Fatal("restore followed a manifest symlink")
	}
}

func TestRestoreManifestDigestRejectsExcessiveSize(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "live")
	if err := os.Mkdir(live, 0700); err != nil {
		t.Fatal(err)
	}
	raw := append([]byte(`{"author":"creator","postId":"post-a"}`),
		bytes.Repeat([]byte(" "), maxArchiveManifestBytes)...)
	if err := os.WriteFile(filepath.Join(live, "manifest.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := revisionRestoreManifestDigest(live, "creator", "post-a"); err == nil {
		t.Fatal("restore accepted an oversized manifest")
	}
}

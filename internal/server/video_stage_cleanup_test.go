package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAbandonedVideoStagingCleanedBeforeStorageStartup(t *testing.T) {
	root := t.TempDir()
	orphan := filepath.Join(root, ".video-stage-123456")
	if err := os.Mkdir(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "rendition.mp4"), []byte("incomplete video"), 0600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(root, "my-video-stage-123456")
	if err := os.Mkdir(unrelated, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, testKey(t)); err != nil {
		t.Fatalf("storage startup refused ordinary abandoned encoder output: %v", err)
	}
	if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
		t.Fatalf("staging data was not reclaimed: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal("unrelated directory was removed")
	}
}

func TestAbandonedVideoStagingRejectsSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	sentinel := filepath.Join(outside, "keep")
	if err := os.WriteFile(sentinel, []byte("owner data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, ".video-stage-987654")); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, testKey(t)); err == nil {
		t.Fatal("untrusted video stage symlink did not block startup")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "owner data" {
		t.Fatalf("staging cleanup touched a foreign directory: %q %v", data, err)
	}
}

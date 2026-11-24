package server

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFileTransactionMetadataHash(t *testing.T) {
	root := t.TempDir()
	data := []byte("{\n  \"job\": {}\n}")
	path := filepath.Join(root, "jobs.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{0, int64(len(data))} {
		got, err := hashFileTransactionMetadata(root, "jobs.json", limit)
		if err != nil || got != metadataDigest(data) {
			t.Fatalf("digest mismatch: %q %v", got, err)
		}
	}
	if _, err := hashFileTransactionMetadata(root, "jobs.json", int64(len(data)-1)); err == nil {
		t.Fatal("oversized metadata accepted")
	}
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	if _, err := hashFileTransactionMetadata(root, "jobs.json", 0); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := hashFileTransactionMetadata(root, "jobs.json", 0); err == nil {
		t.Fatal("FIFO accepted")
	}
}

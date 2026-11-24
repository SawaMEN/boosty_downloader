package server

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRevisionBlockEstimateHardlinksAndSparse(t *testing.T) {
	root := t.TempDir()
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	for _, dir := range []string{first, second} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	unique := filepath.Join(first, "unique")
	shared := filepath.Join(first, "shared")
	for _, path := range []string{unique, shared} {
		if err := os.WriteFile(path, make([]byte, 8192), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(unique, filepath.Join(second, "unique")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(shared, filepath.Join(root, "retained")); err != nil {
		t.Fatal(err)
	}
	sparse := filepath.Join(second, "sparse")
	if err := os.WriteFile(sparse, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(sparse, 1<<30); err != nil {
		t.Fatal(err)
	}
	var want int64
	for _, path := range []string{unique, sparse} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want += info.Sys().(*syscall.Stat_t).Blocks * 512
	}
	e := newRevisionBlockEstimate(root)
	var logical int64
	for _, path := range []string{first, second} {
		n, err := revisionCandidateBytes(context.Background(), root, path, e)
		if err != nil {
			t.Fatal(err)
		}
		logical += n
	}
	if logical != 1<<30+3*8192 {
		t.Fatalf("logical bytes: %d", logical)
	}
	if got, ok := e.bytes(); !ok || got != want {
		t.Fatalf("allocated bytes: %d want %d, valid=%v", got, want, ok)
	}
}

func TestRevisionBlockEstimateBoundedAndDuplicate(t *testing.T) {
	root := t.TempDir()
	e := newRevisionBlockEstimate(root)
	e.limit = 1
	var files []os.FileInfo
	for _, name := range []string{"a", "b"} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, info)
		e.observe(info)
	}
	if _, ok := e.bytes(); ok || e.inodes != nil {
		t.Fatal("inode limit did not discard incomplete estimate")
	}
	e = newRevisionBlockEstimate(root)
	e.observe(files[0])
	e.observe(files[0])
	if _, ok := e.bytes(); ok {
		t.Fatal("duplicate path was accepted as a second hardlink")
	}
}

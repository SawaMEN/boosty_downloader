package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorageUsageCountsOwnerOnlyAndTemporary(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "library", "alice", "post")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "movie.part"), []byte("five!"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "library", "bob"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "library", "bob", "secret"), []byte("other data"), 0600); err != nil {
		t.Fatal(err)
	}
	used, err := userStoredBytes(root, "alice")
	if err != nil || used != 5 {
		t.Fatalf("used=%d err=%v", used, err)
	}
	used, err = userStoredBytes(root, "no-such-owner")
	if err != nil || used != 0 {
		t.Fatalf("missing owner: %d %v", used, err)
	}
	if _, err := userStoredBytes(root, "../bob"); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestStorageUsageRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "library", "alice")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(folder, "foreign")); err != nil {
		t.Fatal(err)
	}
	if _, err := userStoredBytes(root, "alice"); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestStorageQuotaRejectsWriteBeforeItReachesDisk(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "library", "alice")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "full.bin"), []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := withStorageBudget(context.Background(), root, "alice", 0, 1)
	budget := ctx.Value(storageBudgetKey{}).(*storageBudget)
	budget.maxUser = 9 // fixture: 9-byte logical quota
	var written bytes.Buffer
	if _, err := guardedCopy(ctx, &written, strings.NewReader("123")); err == nil {
		t.Fatal("quota exceeded")
	}
	if written.Len() != 0 {
		t.Fatalf("wrote %d bytes before quota check", written.Len())
	}
	if _, err := guardedCopy(ctx, &written, strings.NewReader("!")); err != nil {
		t.Fatal(err)
	}
	if written.String() != "!" {
		t.Fatal("allowed byte lost")
	}
}

func TestStorageGuardEnforcesFreeReserveAndCancellation(t *testing.T) {
	root := t.TempDir()
	_, free, err := filesystemBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := withStorageBudget(context.Background(), root, "alice", 1, 0)
	budget := ctx.Value(storageBudgetKey{}).(*storageBudget)
	budget.minFree = free + 1
	var out bytes.Buffer
	if _, err := guardedCopy(ctx, &out, strings.NewReader("video")); err == nil {
		t.Fatal("free-space reserve bypassed")
	}
	if out.Len() != 0 {
		t.Fatal("data was written despite low free space")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := guardedCopy(canceled, &out, strings.NewReader("x")); err == nil {
		t.Fatal("cancellation ignored")
	}
}

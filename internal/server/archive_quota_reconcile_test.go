package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveQuotaReconcileCountsUntrackedAndHardlinks(t *testing.T) {
	root := t.TempDir()
	live := filepath.Join(root, "library", "owner", "author", "post")
	stage := filepath.Join(root, "library", "owner", "author", ".stage")
	for _, dir := range []string{live, stage} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(live, "media"), []byte("1234"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := withStorageBudget(context.Background(), root, "owner", 0, 1)
	b := ctx.Value(storageBudgetKey{}).(*storageBudget)
	b.maxUser = 10
	if err := checkStorageBudget(ctx, 0); err != nil || b.used != 4 {
		t.Fatalf("initial accounting: %d %v", b.used, err)
	}
	if err := os.Link(filepath.Join(live, "media"), filepath.Join(stage, "media")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "index.html"), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := reconcileArchiveStorageBudget(ctx); err == nil || b.used != 11 {
		t.Fatalf("untracked staging bytes not counted: %d %v", b.used, err)
	}
	if err := os.Remove(filepath.Join(stage, "index.html")); err != nil {
		t.Fatal(err)
	}
	if err := reconcileArchiveStorageBudget(ctx); err != nil || b.used != 8 {
		t.Fatalf("reclaimed bytes not reconciled: %d %v", b.used, err)
	}
}

func TestOwnerStorageTreeSeparateHistoryAndUnsafeParent(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "library", "owner")
	history := filepath.Join(root, "revisions", "owner")
	for _, dir := range []string{lib, history} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(lib, "media"), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(lib, "media"), filepath.Join(history, "media")); err != nil {
		t.Fatal(err)
	}
	for _, tree := range []string{"library", "revisions"} {
		if n, err := ownerStorageTreeBytes(context.Background(), root, tree, "owner"); err != nil || n != 3 {
			t.Fatalf("%s: %d %v", tree, n, err)
		}
	}
	foreign := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "library"), filepath.Join(foreign, "library")); err != nil {
		t.Fatal(err)
	}
	if _, err := userStoredBytes(foreign, "owner"); err == nil {
		t.Fatal("symlinked library parent was traversed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ownerStorageTreeBytes(ctx, root, "revisions", "owner"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestReconcileFailedScanPreservesBaseline(t *testing.T) {
	root := t.TempDir()
	ctx := withStorageBudget(context.Background(), root, "owner", 0, 1)
	b := ctx.Value(storageBudgetKey{}).(*storageBudget)
	b.initialized, b.used = true, 7
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "library")); err != nil {
		t.Fatal(err)
	}
	if err := reconcileArchiveStorageBudget(ctx); err == nil || b.used != 7 {
		t.Fatalf("invalid scan replaced baseline: %d %v", b.used, err)
	}
}

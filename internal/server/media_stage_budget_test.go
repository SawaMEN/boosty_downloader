package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMediaStageBudgetCleanupPreservesPublishedAndCacheBytes(t *testing.T) {
	root := t.TempDir()
	ctx := withStorageBudget(context.Background(), root, "owner", 0, 1)
	b := ctx.Value(storageBudgetKey{}).(*storageBudget)
	b.maxUser = 30
	b.initialized = true
	b.used = 3
	stage := t.TempDir()
	scoped := withMediaStageBudget(ctx)
	write := func(ctx context.Context, path, data string) {
		t.Helper()
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = guardedCopy(ctx, f, strings.NewReader(data))
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("write: %v %v", err, closeErr)
		}
	}
	write(scoped, filepath.Join(stage, "track"), "123456")
	write(scoped, filepath.Join(stage, "segment"), "1234")
	if err := os.Remove(filepath.Join(stage, "segment")); err != nil {
		t.Fatal(err)
	}
	releaseStorageBudget(scoped, 4)
	cache := filepath.Join(root, "cache")
	write(withoutMediaStageBudget(scoped), cache, "12345")
	output := filepath.Join(stage, "remux.mp4")
	if err := os.WriteFile(output, []byte("12345678"), 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "final.mp4")
	if err := publishStagedVideo(scoped, output, dest, 8); err != nil {
		t.Fatal(err)
	}
	if b.used != 22 {
		t.Fatalf("before cleanup used=%d", b.used)
	}
	cleanupMediaStage(scoped, stage)
	cleanupMediaStage(scoped, stage)
	if b.used != 16 {
		t.Fatalf("cleanup lost retained bytes or credited twice: %d", b.used)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("stage remains")
	}
}

func TestMediaStageBudgetRejectsUnchargedFinalOutput(t *testing.T) {
	root := t.TempDir()
	ctx := withStorageBudget(context.Background(), root, "owner", 0, 1)
	b := ctx.Value(storageBudgetKey{}).(*storageBudget)
	b.initialized = true
	b.maxUser = 10
	b.used = 8
	stage := t.TempDir()
	output := filepath.Join(stage, "new.mp4")
	dest := filepath.Join(root, "old.mp4")
	if err := os.WriteFile(output, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishStagedVideo(withMediaStageBudget(ctx), output, dest, 3); err == nil {
		t.Fatal("publication exceeded quota")
	}
	raw, err := os.ReadFile(dest)
	if err != nil || string(raw) != "old" || b.used != 8 {
		t.Fatal("failed publication modified old file or budget")
	}
}

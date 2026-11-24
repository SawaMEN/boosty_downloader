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

type chunkRecordingWriter struct {
	sizes []int
}

func (w *chunkRecordingWriter) Write(p []byte) (int, error) {
	w.sizes = append(w.sizes, len(p))
	return len(p), nil
}

func TestGuardedCopyBoundsWriterTo(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 2*guardedCopyChunkBytes+17)
	// bytes.Reader implements io.WriterTo; the old CopyBuffer path issued
	// one write for the whole payload, ignoring the supplied buffer.
	var out chunkRecordingWriter
	n, err := guardedCopy(context.Background(), &out, bytes.NewReader(payload))
	if err != nil || n != int64(len(payload)) {
		t.Fatalf("copy: n=%d err=%v", n, err)
	}
	if len(out.sizes) != 3 || out.sizes[0] != guardedCopyChunkBytes ||
		out.sizes[1] != guardedCopyChunkBytes || out.sizes[2] != 17 {
		t.Fatalf("unbounded writes: %v", out.sizes)
	}
}

func TestGuardedCopyCanceledEmptySource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if n, err := guardedCopy(ctx, &out, strings.NewReader("")); n != 0 || err != context.Canceled {
		t.Fatalf("canceled empty copy: n=%d err=%v", n, err)
	}
}

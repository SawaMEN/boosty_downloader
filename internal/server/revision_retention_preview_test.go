package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRevisionRetentionPreviewDoesNotDelete(t *testing.T) {
	store, err := newStorage(t.TempDir(), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	job := Job{ID: "abcdefabcdefabcdefabcdef", OwnerID: "admin", Author: "creator", PostID: "post1", State: "done"}
	base := store.archiveDir(job.OwnerID, job.Author, job.PostID)
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(base, "index.html"), []byte("HTML")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		man := ArchiveManifest{Author: job.Author, PostID: job.PostID, Title: "Snapshot",
			CapturedAt: time.Date(2026, 11, 1, i, 0, 0, 0, time.UTC)}
		if err := writeJSON(filepath.Join(base, "manifest.json"), man); err != nil {
			t.Fatal(err)
		}
		if _, err := store.saveArchiveRevision(job); err != nil {
			t.Fatal(err)
		}
	}
	list, err := store.listArchiveRevisions(job)
	if err != nil || len(list) != 3 {
		t.Fatalf("history: %v %v", list, err)
	}
	var want int64
	for _, rev := range list[1:] {
		for _, name := range []string{"index.html", "manifest.json"} {
			info, err := os.Stat(filepath.Join(store.revisionsDir(job), rev.ID, name))
			if err != nil {
				t.Fatal(err)
			}
			want += info.Size()
		}
	}
	count, size, err := store.previewRevisionCandidates(context.Background(), job, 1)
	if err != nil || count != 2 || size != want {
		t.Fatalf("preview: %d %d want %d: %v", count, size, want, err)
	}
	after, err := store.listArchiveRevisions(job)
	if err != nil || len(after) != 3 {
		t.Fatal("preview changed history")
	}
	if _, _, err := store.previewRevisionCandidates(context.Background(), job, 0); err == nil {
		t.Fatal("invalid limit accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.previewRevisionCandidates(ctx, job, 1); err == nil {
		t.Fatal("canceled preview accepted")
	}
	if err := os.Symlink(base, filepath.Join(store.revisionsDir(job), list[2].ID, "unsafe")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.previewRevisionCandidates(context.Background(), job, 1); err == nil {
		t.Fatal("unsafe candidate reported as measured")
	}
}

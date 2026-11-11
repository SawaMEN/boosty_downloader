package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestJobUpsertRollsBackMemoryAfterFailedAtomicSave(t *testing.T) {
	root := t.TempDir()
	s, err := newStorage(root, testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	id := strings.Repeat("1", 24)
	job := Job{ID: id, OwnerID: "first", Author: "creator", PostID: "post-1",
		Title: "old", State: "queued", CreatedAt: now, UpdatedAt: now}
	if err := s.upsert(job); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(root, "jobs.json")
	saved := filepath.Join(root, "jobs.json.original")
	if err := os.Rename(live, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("jobs.json.original", live); err != nil {
		t.Fatal(err)
	}
	changed := job
	changed.Title = "new and not committed"
	if err := s.upsert(changed); err == nil {
		t.Fatal("unsafe filesystem write unexpectedly succeeded")
	}
	got, ok := s.get(id)
	if !ok || got.Title != "old" {
		t.Fatalf("failed disk write left newer job in memory: %+v", got)
	}
	changed.ID = strings.Repeat("2", 24)
	if err := s.upsert(changed); err == nil {
		t.Fatal("new job unexpectedly persisted")
	}
	if _, ok := s.get(changed.ID); ok {
		t.Fatal("new job remained in memory after failed write")
	}
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, live); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newStorage(root, testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	disk, ok := reloaded.get(id)
	if !ok || disk.Title != "old" {
		t.Fatalf("disk image differs from memory after failed write: %+v", disk)
	}
}

func TestAuthorImportUpdateRollsBackMemoryAfterFailedSave(t *testing.T) {
	root := t.TempDir()
	s, err := newStorage(root, testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	id := strings.Repeat("a", 24)
	imported := AuthorImport{ID: id, OwnerID: "first", Author: "creator",
		State: "done", CreatedAt: now, UpdatedAt: now}
	if _, err := s.createImports([]AuthorImport{imported}); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(root, "imports.json")
	saved := filepath.Join(root, "imports.json.original")
	if err := os.Rename(live, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("imports.json.original", live); err != nil {
		t.Fatal(err)
	}
	changed := imported
	changed.State = "failed"
	if err := s.updateImport(changed); err == nil {
		t.Fatal("symlinked metadata target unexpectedly writable")
	}
	got := s.importsFor("first")
	if len(got) != 1 || got[0].State != "done" {
		t.Fatalf("failed disk write left mutated import in memory: %+v", got)
	}
	if err := os.Remove(live); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, live); err != nil {
		t.Fatal(err)
	}
	reloaded, err := newStorage(root, testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	disk := reloaded.importsFor("first")
	if len(disk) != 1 || disk[0].State != "done" {
		t.Fatalf("failed update changed persisted import: %+v", disk)
	}
}

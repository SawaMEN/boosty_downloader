package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrashRestoreAndPurge(t *testing.T) {
	root := t.TempDir()
	key := testKey(t)
	s, err := newStorage(root, key)
	if err != nil {
		t.Fatal(err)
	}
	j := Job{ID: "123456789012345678901234", OwnerID: "admin", Author: "example", PostID: "post1", Title: "A post", State: "done", SharedForAll: true, SharedWith: []string{"guest"}, AutoSync: true}
	if err := s.upsert(j); err != nil {
		t.Fatal(err)
	}
	folder := s.archiveDir(j.OwnerID, j.Author, j.PostID)
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "index.html"), []byte("archive"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.purgeOwned("admin", j.ID, ""); err == nil {
		t.Fatal("live archive purged")
	}
	if _, err := s.removeOwned("guest", j.ID, ""); err == nil {
		t.Fatal("non-owner trash")
	}
	if n, err := s.removeOwned("admin", j.ID, ""); err != nil || n != 1 {
		t.Fatalf("trash: %d %v", n, err)
	}
	if _, ok := s.get(j.ID); ok || len(s.all()) != 0 || len(s.trashed("admin")) != 1 {
		t.Fatal("trashed archive leaked")
	}
	if len(s.trashed("guest")) != 0 {
		t.Fatal("trash visible to other user")
	}
	if _, _, err := s.createUniqueJob(Job{ID: "abcdef0123456789abcdef01", OwnerID: "admin", Author: j.Author, PostID: j.PostID}); err == nil {
		t.Fatal("overwrite of trashed content")
	}
	again, err := newStorage(root, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.trashed("admin")) != 1 {
		t.Fatal("trash not persisted")
	}
	if _, err := again.restoreOwned("guest", j.ID); err == nil {
		t.Fatal("non-owner restore")
	}
	restored, err := again.restoreOwned("admin", j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.DeletedAt != nil || restored.SharedForAll || len(restored.SharedWith) != 0 || restored.AutoSync {
		t.Fatal("old sharing or sync restored")
	}
	if _, ok := again.get(j.ID); !ok {
		t.Fatal("restore invisible")
	}
	if _, err := os.Stat(filepath.Join(folder, "index.html")); err != nil {
		t.Fatal(err)
	}
	if n, err := again.removeOwned("admin", j.ID, ""); err != nil || n != 1 {
		t.Fatalf("second trash: %d %v", n, err)
	}
	if _, err := again.purgeOwned("guest", j.ID, ""); err == nil {
		t.Fatal("non-owner purge")
	}
	if n, err := again.purgeOwned("admin", j.ID, ""); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Fatal("purged file still present", err)
	}
	if len(again.trashed("admin")) != 0 {
		t.Fatal("purged entry remained")
	}
}

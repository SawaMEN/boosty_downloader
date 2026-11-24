package server

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStorageImportBatchCollisionIsAtomic(t *testing.T) {
	s, err := newStorage(t.TempDir(), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := AuthorImport{ID: "existing", OwnerID: "owner", Author: "first", State: "done", CreatedAt: now, UpdatedAt: now}
	if _, err := s.createImports([]AuthorImport{old}); err != nil {
		t.Fatal(err)
	}
	before := s.importsFor("owner")
	epoch := s.metadataMutationEpoch
	fresh := AuthorImport{ID: "fresh", OwnerID: "owner", Author: "second", State: "queued", CreatedAt: now, UpdatedAt: now}
	collision := fresh
	collision.ID, collision.Author = old.ID, "third"
	if _, err := s.createImports([]AuthorImport{fresh, collision}); err == nil {
		t.Fatal("existing ID accepted")
	}
	if !reflect.DeepEqual(before, s.importsFor("owner")) || s.metadataMutationEpoch != epoch {
		t.Fatal("collision partially changed batch")
	}
	collision.ID = fresh.ID
	if _, err := s.createImports([]AuthorImport{fresh, collision}); err == nil {
		t.Fatal("duplicate ID within batch accepted")
	}
	if !reflect.DeepEqual(before, s.importsFor("owner")) || s.metadataMutationEpoch != epoch {
		t.Fatal("duplicate partially changed batch")
	}
}

func TestStorageCombinedBatchFailurePreservesRecords(t *testing.T) {
	s, err := newStorage(t.TempDir(), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	job := Job{ID: "job", OwnerID: "owner", Author: "author", PostID: "post", State: "done", CreatedAt: now, SharedWith: []string{"member"}}
	item := AuthorImport{ID: "import", OwnerID: "owner", Author: "author", State: "done", CreatedAt: now, UpdatedAt: now}
	if err := s.upsert(job); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createImports([]AuthorImport{item}); err != nil {
		t.Fatal(err)
	}
	beforeJob, _ := s.get(job.ID)
	beforeImports := s.importsFor("owner")
	epoch := s.metadataMutationEpoch
	path := filepath.Join(s.root, "imports.json")
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.setImportSchedule("owner", item.ID, true); err == nil {
		t.Fatal("expected combined write failure")
	}
	afterJob, ok := s.get(job.ID)
	if !ok || !reflect.DeepEqual(beforeJob, afterJob) || !reflect.DeepEqual(beforeImports, s.importsFor("owner")) {
		t.Fatal("combined rollback did not restore exact records")
	}
	if s.metadataMutationEpoch <= epoch {
		t.Fatal("failed batch did not invalidate generation")
	}
}

func TestStorageBatchTrashRollback(t *testing.T) {
	s, err := newStorage(t.TempDir(), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		job := Job{ID: id, OwnerID: "owner", Author: "author", PostID: id, State: "done", CreatedAt: time.Now().UTC(), SharedWith: []string{"member"}, SharedForAll: true, AutoSync: true}
		if err := s.upsert(job); err != nil {
			t.Fatal(err)
		}
	}
	before := map[string]Job{}
	for _, job := range s.all() {
		before[job.ID] = job
	}
	path := filepath.Join(s.root, "jobs.json")
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if count, err := s.removeOwned("owner", "", "author"); err == nil || count != 0 {
		t.Fatalf("failed batch trash: count=%d err=%v", count, err)
	}
	for id, want := range before {
		got, ok := s.get(id)
		if !ok || !reflect.DeepEqual(want, got) {
			t.Fatalf("trash rollback changed %s", id)
		}
	}
}

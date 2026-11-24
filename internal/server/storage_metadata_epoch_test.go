package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStorageMetadataEpochWriteAndRollback(t *testing.T) {
	s, err := newStorage(t.TempDir(), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	initial := s.metadataMutationEpoch
	job := Job{ID: "job", OwnerID: "owner", Author: "author", PostID: "post", State: "queued", CreatedAt: time.Now().UTC()}
	if err := s.upsert(job); err != nil {
		t.Fatal(err)
	}
	committed := s.metadataMutationEpoch
	if committed <= initial {
		t.Fatal("successful write did not invalidate generation")
	}
	if _, ok := s.get(job.ID); !ok {
		t.Fatal("stored job missing")
	}
	_ = s.all()
	if s.metadataMutationEpoch != committed {
		t.Fatal("read changed generation")
	}
	// Make rename fail without modifying the previous in-memory value.
	path := filepath.Join(s.root, "jobs.json")
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	job.Title = "failed update"
	if err := s.upsert(job); err == nil {
		t.Fatal("expected write failure")
	}
	if s.metadataMutationEpoch <= committed {
		t.Fatal("failed write resurrected previous generation")
	}
	if current, ok := s.get(job.ID); !ok || current.Title == job.Title {
		t.Fatal("failed update was not rolled back in memory")
	}
}

func TestStorageMetadataEpochCombinedAndSaturation(t *testing.T) {
	s, err := newStorage(t.TempDir(), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.metadataMutationEpoch
	if err := s.saveImportsLocked(); err != nil {
		t.Fatal(err)
	}
	if s.metadataMutationEpoch != before+1 {
		t.Fatal("import write did not advance generation")
	}
	if err := s.saveJobsImportsLocked(); err != nil {
		t.Fatal(err)
	}
	if s.metadataMutationEpoch != before+2 {
		t.Fatal("combined write did not advance generation")
	}
	s.metadataMutationEpoch = ^uint64(0) - 1
	s.invalidateMetadataEpochLocked()
	s.invalidateMetadataEpochLocked()
	if s.metadataMutationEpoch != ^uint64(0) {
		t.Fatal("epoch wrapped")
	}
}

func TestSQLPrimaryMetadataEpochWrite(t *testing.T) {
	s, meta, root := sqlFirstStorageFixture(t)
	before := s.metadataMutationEpoch
	jobs := s.all()
	if len(jobs) == 0 {
		t.Fatal("missing fixture job")
	}
	job := jobs[0]
	job.Title = "epoch update"
	if err := s.upsert(job); err != nil {
		t.Fatal(err)
	}
	if s.metadataMutationEpoch <= before {
		t.Fatal("SQL write did not invalidate generation")
	}
	assertSQLFirstStorageParity(t, meta, root)
}

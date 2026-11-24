package server

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestStorageShareFailurePreservesWholeRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Storage) (bool, error)
	}{
		{"everyone", func(s *Storage) (bool, error) { return s.setEveryone("job", "owner", true) }},
		{"grant", func(s *Storage) (bool, error) { return s.changeShare("job", "owner", "other", true) }},
		{"revoke", func(s *Storage) (bool, error) { return s.changeShare("job", "owner", "member", false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := newStorage(t.TempDir(), testKey(t))
			if err != nil {
				t.Fatal(err)
			}
			job := Job{ID: "job", OwnerID: "owner", Author: "author", PostID: "post", State: "done", CreatedAt: time.Now().UTC(), SharedWith: []string{"member"}}
			if err := s.upsert(job); err != nil {
				t.Fatal(err)
			}
			before, _ := s.get(job.ID)
			epoch := s.metadataMutationEpoch
			path := filepath.Join(s.root, "jobs.json")
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if changed, err := tc.change(s); err == nil || changed {
				t.Fatalf("failed write returned changed=%v err=%v", changed, err)
			}
			after, ok := s.get(job.ID)
			if !ok || !reflect.DeepEqual(before, after) {
				t.Fatalf("rollback changed record: before=%+v after=%+v", before, after)
			}
			if s.metadataMutationEpoch <= epoch {
				t.Fatal("failed write did not invalidate epoch")
			}
		})
	}
}

func TestStorageCreateUniqueJobRejectsIDCollision(t *testing.T) {
	s, err := newStorage(t.TempDir(), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	job := Job{ID: "job", OwnerID: "owner", Author: "author", PostID: "post", State: "queued", CreatedAt: time.Now().UTC()}
	if err := s.upsert(job); err != nil {
		t.Fatal(err)
	}
	before, _ := s.get(job.ID)
	job.PostID = "different-post"
	if _, created, err := s.createUniqueJob(job); err == nil || created {
		t.Fatalf("duplicate ID accepted: created=%v err=%v", created, err)
	}
	after, ok := s.get(job.ID)
	if !ok || !reflect.DeepEqual(before, after) {
		t.Fatal("ID collision overwrote existing job")
	}
}

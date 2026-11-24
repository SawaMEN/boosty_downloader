package server

import (
	"fmt"
	"strings"
	"testing"
)

func TestRevisionPreviewBatchIdentityAndSQLDrift(t *testing.T) {
	s, meta, _ := sqlFirstStorageFixture(t)
	job, ok := s.get(strings.Repeat("1", 24))
	if !ok {
		t.Fatal("missing fixture")
	}
	job.State = "done"
	if err := s.upsert(job); err != nil {
		t.Fatal(err)
	}
	identity := newRevisionPreviewIdentity(job)
	wrong := identity
	wrong.owner = "wrong-owner"
	missing := identity
	missing.id = strings.Repeat("f", 24)
	batch := []revisionPreviewIdentity{identity, wrong, missing}
	valid, err := s.verifyRevisionPreviewBatch(batch)
	if err != nil || valid != 1 {
		t.Fatalf("verified mask: %b %v", valid, err)
	}
	if _, err := s.verifyRevisionPreviewBatch(make([]revisionPreviewIdentity, revisionPreviewBatchSize+1)); err == nil {
		t.Fatal("oversized batch accepted")
	}
	// An unrelated row must still invalidate the complete generation.
	if _, err := meta.db.Exec("UPDATE owner_archives SET title='forged' WHERE archive_id=?", strings.Repeat("2", 24)); err != nil {
		t.Fatal(err)
	}
	if valid, err := s.verifyRevisionPreviewBatch(batch); err == nil || valid != 0 || s.primaryFatal == nil {
		t.Fatal("batch accepted SQL drift after an earlier successful audit")
	}
}

func TestRevisionPreviewBatchChangedIdentity(t *testing.T) {
	s, err := newStorage(t.TempDir(), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	job := Job{ID: "job", OwnerID: "owner", Author: "author", PostID: "post", State: "done"}
	if err := s.upsert(job); err != nil {
		t.Fatal(err)
	}
	identity := newRevisionPreviewIdentity(job)
	current, ok := s.get(job.ID)
	if !ok {
		t.Fatal("missing job")
	}
	identity = newRevisionPreviewIdentity(current)
	current.State = "running"
	if err := s.upsert(current); err != nil {
		t.Fatal(err)
	}
	if valid, err := s.verifyRevisionPreviewBatch([]revisionPreviewIdentity{identity}); err != nil || valid != 0 {
		t.Fatalf("running archive accepted: %b %v", valid, err)
	}
}

func BenchmarkRevisionPreviewBatch(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("archives-%d", n), func(b *testing.B) {
			s := sqlScaleFixture(b, n)
			batch := make([]revisionPreviewIdentity, revisionPreviewBatchSize)
			for i := range batch {
				job := s.jobs[fmt.Sprintf("%024x", i+1)]
				batch[i] = newRevisionPreviewIdentity(*job)
			}
			b.Run("point-reads", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for _, identity := range batch {
						if _, ok := s.get(identity.id); !ok {
							b.Fatal("missing job")
						}
					}
				}
			})
			b.Run("two-batch-audits", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					for pass := 0; pass < 2; pass++ {
						if valid, err := s.verifyRevisionPreviewBatch(batch); err != nil || valid != ^uint32(0) {
							b.Fatalf("invalid batch: %b %v", valid, err)
						}
					}
				}
			})
		})
	}
}

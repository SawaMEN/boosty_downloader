package server

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSQLPrimaryVerifiedProjectionDoesNotAlias(t *testing.T) {
	s, _, _ := sqlFirstStorageFixture(t)
	id := strings.Repeat("1", 24)
	j, ok := s.get(id)
	if !ok {
		t.Fatal("missing archive")
	}
	original := cloneStorageJob(j)
	if len(j.SharedWith) == 0 {
		t.Fatal("fixture lacks grants")
	}
	j.SharedWith[0] = "forged"
	j.Title = "changed"
	got, ok := s.get(id)
	if !ok || !sqliteShadowEqual(got, original) {
		t.Fatal("point read mutated canonical archive")
	}
	for _, item := range s.all() {
		if item.ID == id {
			item.SharedWith[0] = "another-forgery"
		}
	}
	got, ok = s.get(id)
	if !ok || !sqliteShadowEqual(got, original) {
		t.Fatal("inventory read mutated canonical archive")
	}
}

func TestSQLPrimaryVerifiedProjectionsCheckUnrelatedRows(t *testing.T) {
	for _, mode := range []string{"get", "all", "queued", "imports", "queued-imports", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			s, meta, _ := sqlFirstStorageFixture(t)
			// A corrupt archive outside the requested owner's import view
			// must still quarantine the entire SQL-primary generation.
			if _, err := meta.db.Exec("UPDATE owner_archives SET title='forged' WHERE archive_id=?", strings.Repeat("2", 24)); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "get":
				if _, ok := s.get(strings.Repeat("1", 24)); ok {
					t.Fatal("point read survived unrelated drift")
				}
			case "all":
				if len(s.all()) != 0 {
					t.Fatal("inventory survived unrelated drift")
				}
			case "queued":
				if len(s.queued()) != 0 {
					t.Fatal("queue survived unrelated drift")
				}
			case "imports":
				if len(s.importsFor("first")) != 0 {
					t.Fatal("imports survived unrelated drift")
				}
			case "queued-imports":
				if len(s.queuedImports()) != 0 {
					t.Fatal("import queue survived unrelated drift")
				}
			case "canceled":
				if !s.importCanceled(strings.Repeat("a", 24)) {
					t.Fatal("import survived unrelated drift")
				}
			}
			if s.primaryFatal == nil {
				t.Fatal("projection did not quarantine corrupt generation")
			}
		})
	}
}

func TestSQLPrimaryVerifiedProjectionParity(t *testing.T) {
	s, _, _ := sqlFirstStorageFixture(t)
	jobs, imports, err := s.sqlitePrimaryReadSnapshotLocked()
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range jobs {
		got, ok := s.get(id)
		if want.DeletedAt == nil && (!ok || !sqliteShadowEqual(got, want)) {
			t.Fatalf("point projection differs for %s", id)
		}
	}
	for _, got := range s.all() {
		if !sqliteShadowEqual(got, jobs[got.ID]) {
			t.Fatal("archive projection differs")
		}
	}
	for _, got := range s.importsFor("first") {
		if !sqliteShadowEqual(got, imports[got.ID]) {
			t.Fatal("import projection differs")
		}
	}
	// Import results must also be detached from the canonical map.
	item := imports[strings.Repeat("a", 24)]
	item.Subscription = &CapturedSubscription{}
	item.UpdatedAt = time.Now().UTC()
	if err := s.updateImport(item); err != nil {
		t.Fatal(err)
	}
	before := s.importsFor("first")
	after := s.importsFor("first")
	if len(after) != 1 || after[0].Subscription == nil {
		t.Fatal("missing import subscription")
	}
	// A returned pointer must be separate from the canonical object.
	if after[0].Subscription == s.imports[item.ID].Subscription {
		t.Fatal("import subscription aliases canonical state")
	}
	after[0].Subscription = nil
	if !reflect.DeepEqual(before, s.importsFor("first")) {
		t.Fatal("import projection mutated canonical state")
	}
}

func BenchmarkSQLPrimaryPointProjection(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("archives-%d", n), func(b *testing.B) {
			s := sqlScaleFixture(b, n)
			id := fmt.Sprintf("%024x", n/2)
			b.Run("materialized", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					s.mu.Lock()
					jobs, _, err := s.sqlitePrimaryReadSnapshotLocked()
					job := jobs[id]
					s.mu.Unlock()
					if err != nil || job.ID != id {
						b.Fatal("invalid point snapshot", err)
					}
				}
			})
			b.Run("verified-projection", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if job, ok := s.get(id); !ok || job.ID != id {
						b.Fatal("invalid verified point projection")
					}
				}
			})
		})
	}
}

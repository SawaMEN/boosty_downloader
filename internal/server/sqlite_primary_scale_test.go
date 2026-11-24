package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLPrimaryStreamingGuardRejectsDrift(t *testing.T) {
	for _, scenario := range []string{"missing-job", "wrong-memory", "missing-import", "forged-grant", "import-index", "unknown-sql-job"} {
		t.Run(scenario, func(t *testing.T) {
			s, meta, _ := sqlFirstStorageFixture(t)
			if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("1", 24)
			switch scenario {
			case "missing-job":
				delete(s.jobs, id)
			case "wrong-memory":
				s.jobs[id].Title = "wrong memory"
			case "missing-import":
				delete(s.imports, strings.Repeat("a", 24))
			case "forged-grant":
				if _, err := meta.db.Exec("INSERT INTO archive_grants(archive_id,grantee_owner_id,created_at) VALUES(?,?,?)", id, "forged-member", sqliteDate(time.Now())); err != nil {
					t.Fatal(err)
				}
			case "import-index":
				if _, err := meta.db.Exec("UPDATE author_imports SET job_state='canceled' WHERE import_id=?", strings.Repeat("a", 24)); err != nil {
					t.Fatal(err)
				}
			case "unknown-sql-job":
				job := *s.jobs[id]
				job.ID = strings.Repeat("e", 24)
				job.PostID = "new-post"
				if err := meta.apply(context.Background(), []sqliteArchiveChange{{After: job}}, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.sqlitePrimaryGuardMutationLocked(); err == nil || s.primaryFatal == nil {
				t.Fatal("streaming guard accepted drift")
			}
		})
	}
}

// Synthetic metadata-only library: real schema, real SQL projections, one
// grant per job. Population is outside timed regions. No network or media.
func sqlScaleFixture(b *testing.B, n int) *Storage {
	b.Helper()
	schema, err := os.ReadFile("../../deploy/sqlite/schema_v1.sql")
	if err != nil {
		b.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(b.TempDir(), "scale.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(string(schema)); err != nil {
		b.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	post, err := tx.Prepare("INSERT INTO source_posts(post_key,author_slug,remote_post_id,first_seen_at) VALUES(?,?,?,?)")
	if err != nil {
		b.Fatal(err)
	}
	defer post.Close()
	archive, err := tx.Prepare("INSERT INTO owner_archives(archive_id,owner_id,post_key,title,job_state,created_at,updated_at,archived_at,auto_sync,shared_for_all,payload_json) VALUES(?,?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		b.Fatal(err)
	}
	defer archive.Close()
	grant, err := tx.Prepare("INSERT INTO archive_grants(archive_id,grantee_owner_id,created_at) VALUES(?,?,?)")
	if err != nil {
		b.Fatal(err)
	}
	defer grant.Close()
	s := &Storage{primary: &sqliteMetadata{db: db}, jobs: make(map[string]*Job, n), imports: map[string]*AuthorImport{}}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	date := sqliteDate(now)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("%024x", i)
		j := Job{ID: id, OwnerID: "owner", Author: "creator", PostID: id, Title: "Archived media", State: "done", CreatedAt: now, UpdatedAt: now, ArchiveUpdatedAt: now, SharedWith: []string{"member"}}
		payload, err := json.Marshal(j)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := post.Exec(i, j.Author, j.PostID, date); err != nil {
			b.Fatal(err)
		}
		if _, err := archive.Exec(j.ID, j.OwnerID, i, j.Title, j.State, date, date, date, 0, 0, string(payload)); err != nil {
			b.Fatal(err)
		}
		if _, err := grant.Exec(j.ID, "member", date); err != nil {
			b.Fatal(err)
		}
		s.jobs[id] = &j
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return s
}

func BenchmarkSQLPrimaryLibraryGuard(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("archives-%d", n), func(b *testing.B) {
			s := sqlScaleFixture(b, n)
			b.Run("materialized", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := s.sqlitePrimaryReadSnapshotLocked(); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("streaming", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

func TestSQLPrimaryOrderedGrantScan(t *testing.T) {
	for _, scenario := range []string{"valid", "missing", "extra", "orphan-first", "orphan-last", "orphan-post"} {
		t.Run(scenario, func(t *testing.T) {
			s, meta, _ := sqlFirstStorageFixture(t)
			id := strings.Repeat("1", 24)
			switch scenario {
			case "missing":
				if _, err := meta.db.Exec("DELETE FROM archive_grants WHERE archive_id=?", id); err != nil {
					t.Fatal(err)
				}
			case "extra":
				if _, err := meta.db.Exec("INSERT INTO archive_grants VALUES(?,?,?)", id, "extra-member", sqliteDate(time.Now())); err != nil {
					t.Fatal(err)
				}
			case "orphan-first", "orphan-last", "orphan-post":
				if _, err := meta.db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
					t.Fatal(err)
				}
				if scenario == "orphan-post" {
					if _, err := meta.db.Exec("UPDATE owner_archives SET post_key=-1 WHERE archive_id=?", id); err != nil {
						t.Fatal(err)
					}
				} else {
					orphan := strings.Repeat("0", 24)
					if scenario == "orphan-last" {
						orphan = strings.Repeat("z", 24)
					}
					if _, err := meta.db.Exec("INSERT INTO archive_grants VALUES(?,?,?)", orphan, "member", sqliteDate(time.Now())); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := meta.db.Exec("PRAGMA foreign_keys=ON"); err != nil {
					t.Fatal(err)
				}
			}
			err := s.sqlitePrimaryGuardMutationLocked()
			if scenario == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || s.primaryFatal == nil {
				t.Fatal("ordered scan accepted ACL or identity drift")
			}
		})
	}
}

// Reusing SQL scan destinations and RawBytes must never reuse decoded
// metadata, especially optional fields and ACL slices across rows.
func TestSQLPrimaryCursorScanRowsDetached(t *testing.T) {
	s, _, _ := sqlFirstStorageFixture(t)
	s.mu.Lock()
	first, imports, err := s.sqlitePrimaryReadSnapshotLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 2 || len(imports) == 0 {
		t.Fatal("fixture lacks multiple archives or an import")
	}
	id := strings.Repeat("1", 24)
	otherID := strings.Repeat("2", 24)
	job, ok := first[id]
	if !ok || len(job.SharedWith) == 0 {
		t.Fatal("fixture lacks ACL")
	}
	other, ok := first[otherID]
	if !ok {
		t.Fatal("missing second archive")
	}
	original := cloneStorageJob(job)
	originalOther := cloneStorageJob(other)
	job.SharedWith[0] = "changed"
	if !sqliteShadowEqual(first[otherID], originalOther) {
		t.Fatal("decoded rows share ACL storage")
	}
	s.mu.Lock()
	second, secondImports, err := s.sqlitePrimaryReadSnapshotLocked()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !sqliteShadowEqual(second[id], original) || !sqliteShadowEqual(second[otherID], originalOther) {
		t.Fatal("cursor reuse or projection mutation altered later reads")
	}
	if !sqliteShadowEqual(imports, secondImports) {
		t.Fatal("import cursor changed metadata")
	}
}

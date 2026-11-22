package server

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLFirstStorageReadsSQLForArchiveAndImportViews(t *testing.T) {
	s, meta, _ := sqlFirstStorageFixture(t)
	jobID := strings.Repeat("1", 24)
	if job, ok := s.get(jobID); !ok || job.OwnerID != "first" {
		t.Fatalf("SQL-primary point read lost owner: %+v, %v", job, ok)
	}
	if got := s.all(); len(got) != 2 {
		t.Fatalf("SQL-primary full archive inventory: %+v", got)
	}
	if imports := s.importsFor("first"); len(imports) != 1 || imports[0].Author != "creator" {
		t.Fatalf("SQL-primary import owner view: %+v", imports)
	}
	if imports := s.importsFor("second"); len(imports) != 0 {
		t.Fatalf("SQL-primary import view leaked another owner: %+v", imports)
	}
	if got := s.queued(); len(got) != 0 {
		t.Fatalf("SQL-primary returned unexpected queued archives: %v", got)
	}
	if got := s.queuedImports(); len(got) != 0 {
		t.Fatalf("SQL-primary returned unexpected queued imports: %v", got)
	}
	if s.importCanceled(strings.Repeat("a", 24)) {
		t.Fatal("existing SQL import incorrectly marked canceled")
	}
	if !s.importCanceled(strings.Repeat("9", 24)) {
		t.Fatal("unknown import must fail closed")
	}

	j, ok := s.get(jobID)
	if !ok {
		t.Fatal("missing SQL archive")
	}
	j.State = "queued"
	if err := s.upsert(j); err != nil {
		t.Fatal(err)
	}
	if got := s.queued(); len(got) != 1 || got[0] != jobID {
		t.Fatalf("SQL-primary queue did not reflect committed SQL: %v", got)
	}
	if got, err := meta.job(context.Background(), "first", jobID); err != nil || got.State != "queued" {
		t.Fatalf("SQL-primary queue differs from owner-qualified SQL row: %+v, %v", got, err)
	}
}

func TestSQLFirstStorageReadsQuarantineOnForgedACLAndDivergence(t *testing.T) {
	for _, scenario := range []string{"grant", "column", "memory", "sql-missing", "import-index"} {
		t.Run(scenario, func(t *testing.T) {
			s, meta, root := sqlFirstStorageFixture(t)
			before, err := os.ReadFile(filepath.Join(root, "jobs.json"))
			if err != nil {
				t.Fatal(err)
			}
			id := strings.Repeat("1", 24)
			switch scenario {
			case "grant":
				_, err = meta.db.Exec("INSERT INTO archive_grants(archive_id,grantee_owner_id,created_at) VALUES(?,?,?)",
					id, "forged-member", "2026-11-11T12:00:00Z")
			case "column":
				_, err = meta.db.Exec("UPDATE owner_archives SET shared_for_all=1 WHERE archive_id=?", id)
			case "memory":
				s.jobs[id].Title = "uncommitted in-memory change"
			case "sql-missing":
				_, err = meta.db.Exec("DELETE FROM owner_archives WHERE archive_id=?", id)
			case "import-index":
				_, err = meta.db.Exec("UPDATE author_imports SET job_state='queued' WHERE import_id=?", strings.Repeat("a", 24))
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := s.get(id); ok || got.ID != "" {
				t.Fatal("SQL-primary failed to close archive reads after an ACL or metadata inconsistency")
			}
			if got := s.all(); len(got) != 0 {
				t.Fatal("SQL-primary did not close the archive catalog after quarantine")
			}
			if got := s.importsFor("first"); len(got) != 0 {
				t.Fatal("SQL-primary leaked imports after quarantine")
			}
			if got := s.queued(); len(got) != 0 {
				t.Fatal("SQL-primary did not stop archive scheduling after quarantine")
			}
			if got := s.queuedImports(); len(got) != 0 {
				t.Fatal("SQL-primary did not stop import scheduling after quarantine")
			}
			if !s.importCanceled(strings.Repeat("a", 24)) || s.primaryFatal == nil {
				t.Fatal("SQL-primary did not stop an active import after quarantine")
			}
			valid := Job{
				ID: strings.Repeat("9", 24), OwnerID: "first", Author: "creator",
				PostID: "new-post", State: "queued", CreatedAt: time.Now().UTC(),
			}
			if err := s.upsert(valid); err == nil {
				t.Fatal("quarantined SQL-primary accepted new writes")
			}
			after, err := os.ReadFile(filepath.Join(root, "jobs.json"))
			if err != nil || string(before) != string(after) {
				t.Fatal("SQL-primary read quarantine modified authoritative JSON")
			}
		})
	}
}

func TestSQLFirstStorageRejectsUnexpectedNewSQLArchive(t *testing.T) {
	s, meta, _ := sqlFirstStorageFixture(t)
	old, err := meta.job(context.Background(), "first", strings.Repeat("1", 24))
	if err != nil {
		t.Fatal(err)
	}
	newer := old
	newer.ID = strings.Repeat("9", 24)
	newer.PostID = "sql-only-post"
	newer.SharedWith = nil
	if err := meta.apply(context.Background(), []sqliteArchiveChange{{After: newer}}, nil); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.get(newer.ID); ok || got.ID != "" {
		t.Fatal("unreconciled SQL-only archive was served without startup recovery")
	}
	if s.primaryFatal == nil {
		t.Fatal("SQL-only archive was not quarantined pending reconciliation")
	}
}

func TestSQLPrimarySnapshotRejectsClosedSQLite(t *testing.T) {
	s, meta, _ := sqlFirstStorageFixture(t)
	if err := meta.close(); err != nil {
		t.Fatal(err)
	}
	if len(s.all()) != 0 || s.primaryFatal == nil {
		t.Fatal("closed SQL backend fell back to in-memory archives")
	}
	var err error
	_, err = meta.job(context.Background(), "first", strings.Repeat("1", 24))
	if err == nil || err == sql.ErrNoRows {
		t.Fatal("closed SQLite unexpectedly served an archive")
	}
}

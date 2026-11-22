package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sqlFirstCorruptArchiveIndex(t *testing.T, m *sqliteMetadata) {
	t.Helper()
	if _, err := m.db.Exec("UPDATE owner_archives SET title='untrusted-SQL-index' WHERE archive_id=?", strings.Repeat("1", 24)); err != nil {
		t.Fatal(err)
	}
}

func sqlFirstImages(t *testing.T, s *Storage) ([]byte, []byte) {
	t.Helper()
	jobs, err := json.Marshal(s.jobs)
	if err != nil {
		t.Fatal(err)
	}
	imports, err := json.Marshal(s.imports)
	if err != nil {
		t.Fatal(err)
	}
	return jobs, imports
}

func TestSQLFirstMutationGuardRejectsStaleMemoryBeforeChanges(t *testing.T) {
	id := strings.Repeat("1", 24)
	importID := strings.Repeat("a", 24)
	for _, tc := range []struct {
		name string
		call func(*Storage) error
	}{
		{"upsert", func(s *Storage) error {
			j := *s.jobs[id]
			j.Title = "stale write"
			return s.upsert(j)
		}},
		{"share public", func(s *Storage) error { _, err := s.setEveryone(id, "first", true); return err }},
		{"share direct", func(s *Storage) error { _, err := s.changeShare(id, "first", "third", true); return err }},
		{"queue archive", func(s *Storage) error { _, err := s.transitionToQueued(id, "first"); return err }},
		{"unique archive", func(s *Storage) error {
			_, _, err := s.createUniqueJob(Job{ID: strings.Repeat("f", 24), OwnerID: "first", Author: "creator", PostID: "new", State: "queued", CreatedAt: time.Now().UTC()})
			return err
		}},
		{"trash archive", func(s *Storage) error { _, err := s.removeOwned("first", id, ""); return err }},
		{"restore archive", func(s *Storage) error { _, err := s.restoreOwned("first", id); return err }},
		{"create import", func(s *Storage) error {
			now := time.Now().UTC()
			_, err := s.createImports([]AuthorImport{{ID: strings.Repeat("b", 24), OwnerID: "first", Author: "new", State: "queued", CreatedAt: now, UpdatedAt: now}})
			return err
		}},
		{"cancel import", func(s *Storage) error { return s.cancelImport("first", importID) }},
		{"schedule", func(s *Storage) error { return s.setImportSchedule("first", importID, true) }},
		{"update import", func(s *Storage) error {
			v := *s.imports[importID]
			v.AutoSync = true
			return s.updateImport(v)
		}},
		{"automatic scan", func(s *Storage) error {
			_, err := s.scheduleDueImportsAllowed(time.Now().Add(24*time.Hour), time.Hour, 1, nil)
			return err
		}},
		{"revision restore", func(s *Storage) error {
			_, err := s.restoreArchiveRevision("first", id, strings.Repeat("f", 24))
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, meta, root := sqlFirstStorageFixture(t)
			oldJobs, oldImports := sqlFirstImages(t, s)
			jobDisk, err := os.ReadFile(filepath.Join(root, "jobs.json"))
			if err != nil {
				t.Fatal(err)
			}
			importDisk, err := os.ReadFile(filepath.Join(root, "imports.json"))
			if err != nil {
				t.Fatal(err)
			}
			sqlFirstCorruptArchiveIndex(t, meta)
			if err := tc.call(s); err == nil {
				t.Fatal("SQL-first operation accepted divergent SQL index")
			}
			if s.primaryFatal == nil {
				t.Fatal("SQL-first operation did not quarantine stale memory before mutation")
			}
			newJobs, newImports := sqlFirstImages(t, s)
			if !bytes.Equal(oldJobs, newJobs) || !bytes.Equal(oldImports, newImports) {
				t.Fatal("SQL-first operation mutated memory before verifying the SQL generation")
			}
			newJobDisk, _ := os.ReadFile(filepath.Join(root, "jobs.json"))
			newImportDisk, _ := os.ReadFile(filepath.Join(root, "imports.json"))
			if !bytes.Equal(jobDisk, newJobDisk) || !bytes.Equal(importDisk, newImportDisk) {
				t.Fatal("SQL-first operation modified JSON mirrors under SQL divergence")
			}
		})
	}
}

func TestSQLFirstTrashReadsAndPermanentPurgeFailClosed(t *testing.T) {
	s, meta, root := sqlFirstStorageFixture(t)
	id := strings.Repeat("1", 24)
	if _, err := s.removeOwned("first", id, ""); err != nil {
		t.Fatal(err)
	}
	job := s.jobs[id]
	archive := s.archiveDir(job.OwnerID, job.Author, job.PostID)
	if err := os.MkdirAll(archive, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(archive, "keep.txt")
	if err := os.WriteFile(keep, []byte("retain original archive"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	sqlFirstCorruptArchiveIndex(t, meta)
	if got := s.trashed("first"); len(got) != 0 {
		t.Fatal("trash listing exposed stale SQL-primary metadata")
	}
	if _, ok := s.trashedJob("first", id); ok {
		t.Fatal("trash lookup exposed stale SQL-primary metadata")
	}
	if _, err := s.purgeOwned("first", "", ""); err == nil {
		t.Fatal("bulk physical purge bypassed the SQL-primary generation guard")
	}
	if s.primaryFatal == nil {
		t.Fatal("permanent purge did not quarantine diverged SQL-primary")
	}
	if actual, err := os.ReadFile(keep); err != nil || string(actual) != "retain original archive" {
		t.Fatalf("permanent purge modified original files: %q %v", actual, err)
	}
	after, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("permanent purge mutated JSON metadata after SQL divergence")
	}
}

func TestSQLFirstWorkerClaimsDoNotAdvanceStaleGenerations(t *testing.T) {
	s, meta, _ := sqlFirstStorageFixture(t)
	id := strings.Repeat("1", 24)
	importID := strings.Repeat("a", 24)
	j, ok := s.get(id)
	if !ok {
		t.Fatal("missing archive")
	}
	j.State = "queued"
	if err := s.upsert(j); err != nil {
		t.Fatal(err)
	}
	v := s.importsFor("first")[0]
	v.State = "queued"
	if err := s.updateImport(v); err != nil {
		t.Fatal(err)
	}
	sqlFirstCorruptArchiveIndex(t, meta)
	if _, ok := s.claimQueued(id); ok {
		t.Fatal("archive worker started with invalid SQL index")
	}
	if _, ok := s.claimImport(importID); ok {
		t.Fatal("import worker started with invalid SQL index")
	}
	if s.primaryFatal == nil {
		t.Fatal("worker claim failed to quarantine the SQL-primary adapter")
	}
	if s.jobs[id].State != "queued" || s.imports[importID].State != "queued" {
		t.Fatal("worker mutated in-memory state despite SQL divergence")
	}
}

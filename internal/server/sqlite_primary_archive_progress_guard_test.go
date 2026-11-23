package server

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLFirstArchiveWriteGuardRejectsDivergentIndexBeforeFiles(t *testing.T) {
	s, meta, root := sqlFirstStorageFixture(t)
	id := strings.Repeat("1", 24)
	job, ok := s.get(id)
	if !ok {
		t.Fatal("missing archive fixture")
	}
	job.State = "running"
	if err := s.upsert(job); err != nil {
		t.Fatal(err)
	}
	if err := s.guardArchiveFileMutation(job); err != nil {
		t.Fatal("matching SQL-first archive incorrectly refused file write:", err)
	}
	folder := s.archiveDir(job.OwnerID, job.Author, job.PostID)
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(folder, "existing-media")
	if err := os.WriteFile(keep, []byte("original media"), 0600); err != nil {
		t.Fatal(err)
	}
	jsonBefore, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	sqlFirstCorruptArchiveIndex(t, meta)
	if err := s.guardArchiveFileMutation(job); err == nil {
		t.Fatal("file mutation allowed despite a forged SQL index")
	}
	if s.primaryFatal == nil {
		t.Fatal("SQL-first storage not quarantined")
	}
	progress := *s.jobs[id]
	progress.DoneFiles = 1
	if err := s.upsert(progress); err == nil {
		t.Fatal("progress advanced after SQL-first quarantine")
	}
	jsonAfter, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil || !bytes.Equal(jsonBefore, jsonAfter) {
		t.Fatal("archive error modified JSON mirror")
	}
	if data, err := os.ReadFile(keep); err != nil || string(data) != "original media" {
		t.Fatal("archive data changed after SQL generation failure")
	}
}

func TestSQLFirstArchiveWriteGuardRejectsWrongIdentityAndJobState(t *testing.T) {
	for _, test := range []struct {
		name string
		alter func(*Job)
	}{
		{"owner", func(j *Job) { j.OwnerID = "other" }},
		{"author", func(j *Job) { j.Author = "other" }},
		{"post", func(j *Job) { j.PostID = "other" }},
		{"state", func(j *Job) { j.State = "queued" }},
		{"missing", func(j *Job) { j.ID = strings.Repeat("f", 24) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _, _ := sqlFirstStorageFixture(t)
			id := strings.Repeat("1", 24)
			j := *s.jobs[id]
			j.State = "running"
			if err := s.upsert(j); err != nil {
				t.Fatal(err)
			}
			test.alter(&j)
			if err := s.guardArchiveFileMutation(j); err == nil {
				t.Fatal("changed archive identity or state accepted before media write")
			}
			if s.primaryFatal == nil {
				t.Fatal("inconsistent SQL-first archive was not quarantined")
			}
		})
	}
}

func TestJSONArchiveWriteGuardDoesNotRequireSQLite(t *testing.T) {
	s, _, _ := sqlFirstStorageFixture(t)
	s.primary = nil
	if err := s.guardArchiveFileMutation(Job{}); err != nil {
		t.Fatal("JSON backend unexpectedly requires SQL archive generation:", err)
	}
}

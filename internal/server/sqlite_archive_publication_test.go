package server

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSQLArchivePublicationCrashWorker(t *testing.T) {
	root := os.Getenv("BOOSTY_TEST_SQL_ARCHIVE_ROOT")
	if root == "" {
		return
	}
	s, err := newStorage(root, os.Getenv("BOOSTY_TEST_SQL_ARCHIVE_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.close()
	s.primary = meta
	job, ok := s.get(strings.Repeat("1", 24))
	if !ok {
		t.Fatal("missing SQL job")
	}
	// The shared audit fixture intentionally starts as failed. Model a
	// real worker's durable claim before the publication fault checkpoints.
	job.State = "running"
	job.Error = ""
	if err := s.upsert(job); err != nil {
		t.Fatal(err)
	}
	stage := archiveUpdateWriteStage(t, s, job)
	s.archiveUpdateCheckpoint = func(phase string) error {
		if phase == os.Getenv("BOOSTY_TEST_SQL_ARCHIVE_PHASE") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	_, err = s.publishArchiveUpdate(job, stage)
	t.Fatalf("crash worker escaped checkpoint: %v", err)
}

func TestSQLArchivePublicationRecoversSIGKILL(t *testing.T) {
	for _, phase := range []string{"journal", "old-moved", "new-moved", "primary-prepared", "primary-sql-committed", "primary-json-committed", "metadata", "committed"} {
		t.Run(phase, func(t *testing.T) {
			root, id, hist, _ := sqliteSnapshotAuditFixture(t)
			key := testKey(t)
			before, err := sqliteShadowReadJobs(filepath.Join(root, "jobs.json"))
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSQLArchivePublicationCrashWorker$")
			cmd.Env = append(os.Environ(), "BOOSTY_TEST_SQL_ARCHIVE_ROOT="+root,
				"BOOSTY_TEST_SQL_ARCHIVE_KEY="+key, "BOOSTY_TEST_SQL_ARCHIVE_PHASE="+phase)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("worker did not crash: %v %s", err, out)
			}
			ws, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
				t.Fatalf("unexpected worker failure: %v %s", err, out)
			}
			s, err := newStorage(root, key)
			if err != nil {
				t.Fatal("startup recovery:", err)
			}
			job, ok := s.get(id)
			if !ok {
				t.Fatal("recovered job missing")
			}
			committed := phase == "primary-sql-committed" || phase == "primary-json-committed" || phase == "metadata" || phase == "committed"
			live := s.archiveDir(job.OwnerID, job.Author, job.PostID)
			if committed {
				if job.Title != "New" || job.DoneFiles != 1 {
					t.Fatal("committed SQL metadata lost", job)
				}
				if raw, err := os.ReadFile(filepath.Join(live, "media", "photo.jpg")); err != nil || string(raw) != "new media bytes" {
					t.Fatal("committed media lost", err)
				}
			} else if job.Title != before[id].Title {
				t.Fatal("uncommitted job changed", job)
			}
			if !sqliteShadowEqual(job.SharedWith, before[id].SharedWith) || job.SharedForAll != before[id].SharedForAll {
				t.Fatal("ACL changed", job)
			}
			if err := verifyJSONSQLiteAgreement(root); err != nil {
				t.Fatal("SQL and JSON diverged", err)
			}
			// Offline inventory intentionally uses an immutable checkpointed
			// database. Confirm startup parity first with WAL, then checkpoint
			// SQLite before the separate full media audit.
			meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			var busy, logFrames, checkpointed int
			err = meta.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed)
			closeErr := meta.close()
			if err != nil || closeErr != nil || busy != 0 {
				t.Fatal("cannot checkpoint recovered SQL", err, closeErr, busy)
			}
			if err := verifySQLiteSnapshotInventory(root); err != nil {
				t.Fatal("SQL current revision index and files diverged", err)
			}
			meta, err = openSQLiteMetadata(filepath.Join(root, "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer meta.close()
			var count int
			if err := meta.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM archive_revisions WHERE revision_id=?", hist).Scan(&count); err != nil || count != 1 {
				t.Fatal("history lost", err)
			}
			for _, name := range []string{archiveUpdateJournalName, sqlitePrimaryTransactionName, metadataJournalName} {
				if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
					t.Fatal("journal leaked", name, err)
				}
			}
		})
	}
}

package server

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSQLHistoryDeleteCrashWorker(t *testing.T) {
	root := os.Getenv("BOOSTY_TEST_HISTORY_DELETE_ROOT")
	if root == "" {
		return
	}
	s, err := newStorage(root, os.Getenv("BOOSTY_TEST_HISTORY_DELETE_KEY"))
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
		t.Fatal("missing job")
	}
	s.sqliteHistoryDeleteCheckpoint = func(phase string) error {
		if phase == os.Getenv("BOOSTY_TEST_HISTORY_DELETE_PHASE") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	err = s.deleteSQLHistory(job, strings.Repeat("a", 24))
	t.Fatalf("worker escaped crash checkpoint: %v", err)
}

func TestSQLHistoryDeleteRecoversSIGKILL(t *testing.T) {
	for _, phase := range []string{"prepared", "moved", "indexed", "removed"} {
		t.Run(phase, func(t *testing.T) {
			root, id, hist, media := sqliteSnapshotAuditFixture(t)
			key := testKey(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestSQLHistoryDeleteCrashWorker$")
			cmd.Env = append(os.Environ(), "BOOSTY_TEST_HISTORY_DELETE_ROOT="+root, "BOOSTY_TEST_HISTORY_DELETE_KEY="+key, "BOOSTY_TEST_HISTORY_DELETE_PHASE="+phase)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("worker did not crash: %v %s", err, out)
			}
			ws, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
				t.Fatalf("unexpected worker failure: %v %s", err, out)
			}
			if err := sqliteReindexNoWAL(root); err == nil {
				t.Fatal("offline tool ignored pending deletion")
			}
			s, err := newStorage(root, key)
			if err != nil {
				t.Fatal("startup recovery:", err)
			}
			job, ok := s.get(id)
			if !ok {
				t.Fatal("job lost")
			}
			for _, path := range []string{filepath.Join(s.revisionsDir(job), hist), filepath.Join(s.revisionsDir(job), ".incomplete-delete-"+hist), filepath.Join(root, sqliteHistoryDeleteJournalName)} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("deletion leaked", path, err)
				}
			}
			if _, err := os.Stat(filepath.Join(s.archiveDir(job.OwnerID, job.Author, job.PostID), "media", media)); err != nil {
				t.Fatal("live media lost", err)
			}
			meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			var busy, frames, done int
			err = meta.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &done)
			closeErr := meta.close()
			if err != nil || closeErr != nil || busy != 0 {
				t.Fatal("checkpoint", err, closeErr, busy)
			}
			if err := verifyJSONSQLiteAgreement(root); err != nil {
				t.Fatal("ACL or metadata diverged", err)
			}
			if err := verifySQLiteSnapshotInventory(root); err != nil {
				t.Fatal("media index diverged", err)
			}
			if got := sqliteReindexRevisionCount(t, root); got != 1 {
				t.Fatal("unexpected remaining index rows", got)
			}
		})
	}
}

func TestSQLHistoryDeleteRejectsTamperedRevision(t *testing.T) {
	root, id, hist, media := sqliteSnapshotAuditFixture(t)
	s, err := newStorage(root, testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.close()
	s.primary = meta
	job, ok := s.get(id)
	if !ok {
		t.Fatal("missing job")
	}
	path := filepath.Join(s.revisionsDir(job), hist, "media", media)
	if err := writeAtomic(path, []byte("tampered")); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteSQLHistory(job, hist); err == nil {
		t.Fatal("tampered revision deleted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("tampered file removed", err)
	}
	if _, err := os.Lstat(filepath.Join(root, sqliteHistoryDeleteJournalName)); !os.IsNotExist(err) {
		t.Fatal("invalid intent published")
	}
}

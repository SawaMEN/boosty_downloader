package server

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const purgeCrashTestRoot = "BOOSTY_TEST_PURGE_CRASH_ROOT"
const purgeCrashTestPhase = "BOOSTY_TEST_PURGE_CRASH_PHASE"
const purgeCrashTestKey = "BOOSTY_TEST_PURGE_CRASH_KEY"

func purgeCrashFixture(t *testing.T) (string, string, Job, string, string) {
	t.Helper()
	root := t.TempDir()
	key := testKey(t)
	store, err := newStorage(root, key)
	if err != nil {
		t.Fatal(err)
	}
	job := Job{ID: strings.Repeat("a", 24), OwnerID: "admin", Author: "creator", PostID: "post-a", State: "done"}
	if err := store.upsert(job); err != nil {
		t.Fatal(err)
	}
	folder := store.archiveDir(job.OwnerID, job.Author, job.PostID)
	revisions := store.revisionsDir(job)
	for _, dir := range []string{folder, revisions} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "keep"), []byte("original"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := store.removeOwned("admin", job.ID, ""); err != nil || n != 1 {
		t.Fatalf("cannot trash fixture: %d %v", n, err)
	}
	return root, key, job, folder, revisions
}

func TestPurgeJournalCrashWorker(t *testing.T) {
	root := os.Getenv(purgeCrashTestRoot)
	if root == "" {
		return
	}
	store, err := newStorage(root, os.Getenv(purgeCrashTestKey))
	if err != nil {
		t.Fatal(err)
	}
	phase := os.Getenv(purgeCrashTestPhase)
	store.purgeCheckpoint = func(current string) error {
		if current == phase {
			// A real process death. Deferred filesystem cleanup cannot run.
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	if _, err := store.purgeOwned("admin", strings.Repeat("a", 24), ""); err != nil {
		t.Fatal("purge unexpectedly failed before the kill checkpoint:", err)
	}
	t.Fatal("worker returned without receiving SIGKILL")
}

func purgeCrashAt(t *testing.T, root, key, phase string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPurgeJournalCrashWorker$")
	cmd.Env = append(os.Environ(), purgeCrashTestRoot+"="+root, purgeCrashTestKey+"="+key,
		purgeCrashTestPhase+"="+phase)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ProcessState.Success() {
		t.Fatalf("expected forced SIGKILL at %s; got %v: %s", phase, err, out)
	}
}

func TestPurgeJournalRecoversRealSIGKILLBeforeAndAfterMetadata(t *testing.T) {
	for _, phase := range []string{"journal", "moved", "metadata"} {
		t.Run(phase, func(t *testing.T) {
			root, key, job, folder, revisions := purgeCrashFixture(t)
			purgeCrashAt(t, root, key, phase)
			if _, err := os.Lstat(filepath.Join(root, purgeJournalName)); err != nil {
				t.Fatalf("SIGKILL did not leave the durable purge journal: %v", err)
			}
			store, err := newStorage(root, key)
			if err != nil {
				t.Fatalf("startup cannot recover interrupted purge: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(root, purgeJournalName)); !os.IsNotExist(err) {
				t.Fatal("completed purge recovery retained its transaction journal")
			}
			_, exists := store.jobs[job.ID]
			committed := phase == "metadata"
			if exists == committed {
				t.Fatalf("wrong recovered archive state at %s: exists=%v", phase, exists)
			}
			for _, dir := range []string{folder, revisions} {
				data, err := os.ReadFile(filepath.Join(dir, "keep"))
				if committed {
					if !os.IsNotExist(err) {
						t.Fatalf("committed purge retained private files %s: %v", dir, err)
					}
				} else if err != nil || string(data) != "original" {
					t.Fatalf("uncommitted purge failed to restore %s: %q %v", dir, data, err)
				}
			}
			items, err := os.ReadDir(filepath.Join(root, "trash"))
			if err != nil || len(items) != 0 {
				t.Fatalf("stray staging directories after recovery: %v %v", items, err)
			}
		})
	}
}

func TestPurgeJournalENOSPCBeforeCommitRestoresBothFolders(t *testing.T) {
	for _, phase := range []string{"journal", "moved"} {
		t.Run(phase, func(t *testing.T) {
			root, key, job, folder, revisions := purgeCrashFixture(t)
			store, err := newStorage(root, key)
			if err != nil {
				t.Fatal(err)
			}
			store.purgeCheckpoint = func(now string) error {
				if now == phase {
					return syscall.ENOSPC
				}
				return nil
			}
			if n, err := store.purgeOwned("admin", job.ID, ""); n != 0 || !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("simulated disk-full purge should roll back: n=%d err=%v", n, err)
			}
			if len(store.trashed("admin")) != 1 {
				t.Fatal("uncommitted purge erased in-memory archive")
			}
			for _, dir := range []string{folder, revisions} {
				if _, err := os.Stat(filepath.Join(dir, "keep")); err != nil {
					t.Fatalf("disk-full rollback lost archive directory %s: %v", dir, err)
				}
			}
			if _, err := os.Lstat(filepath.Join(root, purgeJournalName)); !os.IsNotExist(err) {
				t.Fatalf("recoverable failure left a pending journal: %v", err)
			}
		})
	}
}

func TestPurgeJournalFailClosedOnTamperedMetadata(t *testing.T) {
	root, key, _, folder, _ := purgeCrashFixture(t)
	purgeCrashAt(t, root, key, "moved")
	if err := os.WriteFile(filepath.Join(root, "jobs.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, key); err == nil {
		t.Fatal("startup accepted unrecognized archive metadata generation")
	}
	if _, err := os.Lstat(folder); !os.IsNotExist(err) {
		t.Fatal("unknown journal generation unexpectedly restored or deleted a staged archive")
	}
	if _, err := os.Lstat(filepath.Join(root, purgeJournalName)); err != nil {
		t.Fatal("failed recovery removed evidence of incomplete purge", err)
	}
}

func TestPurgeJournalFailClosedOnSymlinkStage(t *testing.T) {
	root, key, _, _, _ := purgeCrashFixture(t)
	purgeCrashAt(t, root, key, "moved")
	raw, err := os.ReadFile(filepath.Join(root, purgeJournalName))
	if err != nil {
		t.Fatal(err)
	}
	var journal purgeJournal
	if err := json.Unmarshal(raw, &journal); err != nil || len(journal.Moves) == 0 {
		t.Fatal("missing durable purge plan", err)
	}
	_, slot, err := purgeMovePaths(root, journal.Moves[0])
	if err != nil {
		t.Fatal(err)
	}
	real := slot + "-original"
	if err := os.Rename(slot, real); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("retain"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, slot); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, key); err == nil {
		t.Fatal("startup followed a replaced purge staging symlink")
	}
	if data, err := os.ReadFile(filepath.Join(outside, "sentinel")); err != nil || string(data) != "retain" {
		t.Fatal("unsafe journal recovery touched outside files")
	}
}

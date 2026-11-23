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
	"time"
)

const restoreCrashRoot = "BOOSTY_RESTORE_CRASH_ROOT"
const restoreCrashKey = "BOOSTY_RESTORE_CRASH_KEY"
const restoreCrashPhase = "BOOSTY_RESTORE_CRASH_PHASE"

func revisionRestoreFixture(t *testing.T) (string, string, Job, string, string) {
	t.Helper()
	root := t.TempDir()
	key := testKey(t)
	s, err := newStorage(root, key)
	if err != nil {
		t.Fatal(err)
	}
	job := Job{
		ID: strings.Repeat("c", 24), OwnerID: "admin", Author: "creator",
		PostID: "post-a", State: "done", Title: "Previous",
		SharedForAll: true, SharedWith: []string{"member"}, AutoSync: true,
	}
	if err := s.upsert(job); err != nil {
		t.Fatal(err)
	}
	live := s.archiveDir(job.OwnerID, job.Author, job.PostID)
	if err := os.MkdirAll(live, 0700); err != nil {
		t.Fatal(err)
	}
	captured := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	old := ArchiveManifest{Author: job.Author, PostID: job.PostID, Title: "Previous", CapturedAt: captured}
	if err := writeJSON(filepath.Join(live, "manifest.json"), old); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(live, "index.html"), []byte("previous HTML")); err != nil {
		t.Fatal(err)
	}
	revision, err := s.saveArchiveRevision(job)
	if err != nil {
		t.Fatal(err)
	}
	newMan := old
	newMan.Title = "Current"
	newMan.CapturedAt = captured.Add(24 * time.Hour)
	if err := writeJSON(filepath.Join(live, "manifest.json"), newMan); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(live, "index.html"), []byte("current HTML")); err != nil {
		t.Fatal(err)
	}
	job.Title = "Current"
	if err := s.upsert(job); err != nil {
		t.Fatal(err)
	}
	return root, key, job, revision, live
}

func TestArchiveRestoreCrashWorker(t *testing.T) {
	root := os.Getenv(restoreCrashRoot)
	if root == "" {
		return
	}
	s, err := newStorage(root, os.Getenv(restoreCrashKey))
	if err != nil {
		t.Fatal(err)
	}
	phase := os.Getenv(restoreCrashPhase)
	s.restoreCheckpoint = func(name string) error {
		if phase == name {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	revisions, err := s.listArchiveRevisions(Job{OwnerID: "admin", Author: "creator", PostID: "post-a"})
	if err != nil || len(revisions) == 0 {
		t.Fatal("no saved source revision", err)
	}
	_, err = s.restoreArchiveRevision("admin", strings.Repeat("c", 24), revisions[len(revisions)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatal("restore worker escaped the kill checkpoint")
}

func revisionRestoreKill(t *testing.T, root, key, phase string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestArchiveRestoreCrashWorker$")
	cmd.Env = append(os.Environ(), restoreCrashRoot+"="+root, restoreCrashKey+"="+key,
		restoreCrashPhase+"="+phase)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ProcessState.Success() {
		t.Fatalf("SIGKILL was not reached at %s: %v %s", phase, err, out)
	}
}

func TestRevisionRestoreRecoversRealSIGKILL(t *testing.T) {
	for _, phase := range []string{"journal", "old-moved", "new-moved", "metadata"} {
		t.Run(phase, func(t *testing.T) {
			root, key, job, _, live := revisionRestoreFixture(t)
			revisionRestoreKill(t, root, key, phase)
			if _, err := os.Stat(filepath.Join(root, revisionRestoreJournalName)); err != nil {
				t.Fatalf("kill left no durable restore journal: %v", err)
			}
			s, err := newStorage(root, key)
			if err != nil {
				t.Fatalf("startup failed to restore a known crash state: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(root, revisionRestoreJournalName)); !os.IsNotExist(err) {
				t.Fatal("recovered journal was not removed")
			}
			committed := phase == "metadata"
			wantTitle, wantHTML := "Current", "current HTML"
			if committed {
				wantTitle, wantHTML = "Previous", "previous HTML"
			}
			actual := s.jobs[job.ID]
			if actual == nil || actual.Title != wantTitle {
				t.Fatalf("incorrect metadata recovery for %s: %+v", phase, actual)
			}
			if committed && (actual.SharedForAll || len(actual.SharedWith) > 0 || actual.AutoSync) {
				t.Fatal("restored version preserved previously granted access")
			}
			if !committed && (!actual.SharedForAll || !actual.AutoSync) {
				t.Fatal("uncommitted version restore modified access grants")
			}
			html, err := os.ReadFile(filepath.Join(live, "index.html"))
			if err != nil || string(html) != wantHTML {
				t.Fatalf("live archive damaged by %s: %q %v", phase, html, err)
			}
			items, err := os.ReadDir(filepath.Dir(live))
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if strings.HasPrefix(item.Name(), ".previous-") || strings.HasPrefix(item.Name(), ".restore-") {
					t.Fatalf("recovered %s left stale work directory %s", phase, item.Name())
				}
			}
		})
	}
}

func TestRevisionRestoreENOSPCRollsBackAndHandlesCommittedResult(t *testing.T) {
	for _, phase := range []string{"journal", "old-moved", "new-moved", "metadata"} {
		t.Run(phase, func(t *testing.T) {
			root, key, job, rev, live := revisionRestoreFixture(t)
			s, err := newStorage(root, key)
			if err != nil {
				t.Fatal(err)
			}
			s.restoreCheckpoint = func(at string) error {
				if at == phase {
					return syscall.ENOSPC
				}
				return nil
			}
			result, err := s.restoreArchiveRevision("admin", job.ID, rev)
			committed := phase == "metadata"
			if committed {
				if err != nil || result.Title != "Previous" {
					t.Fatalf("committed restore misclassified as failure: %+v %v", result, err)
				}
			} else if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("ENOSPC was not returned for uncommitted restore: %v", err)
			}
			want := "current HTML"
			if committed {
				want = "previous HTML"
			}
			data, readErr := os.ReadFile(filepath.Join(live, "index.html"))
			if readErr != nil || string(data) != want {
				t.Fatalf("restore lost original data: %q %v", data, readErr)
			}
			if _, err := os.Lstat(filepath.Join(root, revisionRestoreJournalName)); !os.IsNotExist(err) {
				t.Fatal("successful in-process recovery left its journal")
			}
		})
	}
}

func TestRevisionRestoreIdempotentAfterInterruptedRollback(t *testing.T) {
	root, key, _, _, live := revisionRestoreFixture(t)
	revisionRestoreKill(t, root, key, "new-moved")
	if err := os.RemoveAll(live); err != nil {
		t.Fatal(err)
	}
	s, err := newStorage(root, key)
	if err != nil {
		t.Fatalf("cannot resume interrupted rollback with old backup: %v", err)
	}
	if len(s.jobs) != 1 || s.jobs[strings.Repeat("c", 24)].Title != "Current" {
		t.Fatal("restored rollback did not preserve old JSON generation")
	}
	data, err := os.ReadFile(filepath.Join(live, "index.html"))
	if err != nil || string(data) != "current HTML" {
		t.Fatalf("interrupted rollback lost original files: %q %v", data, err)
	}
}

func TestRevisionRestoreRefusesTamperedMetadata(t *testing.T) {
	root, key, _, _, _ := revisionRestoreFixture(t)
	revisionRestoreKill(t, root, key, "old-moved")
	if err := os.WriteFile(filepath.Join(root, "jobs.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, key); err == nil {
		t.Fatal("startup accepted an unrelated metadata generation")
	}
	if _, err := os.Lstat(filepath.Join(root, revisionRestoreJournalName)); err != nil {
		t.Fatal("invalid journal was erased", err)
	}
}

func TestRevisionRestoreRejectsSymlinkBackup(t *testing.T) {
	root, key, _, _, _ := revisionRestoreFixture(t)
	revisionRestoreKill(t, root, key, "old-moved")
	raw, err := os.ReadFile(filepath.Join(root, revisionRestoreJournalName))
	if err != nil {
		t.Fatal(err)
	}
	var journal revisionRestoreJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		t.Fatal(err)
	}
	_, _, backup, err := revisionRestorePaths(root, journal)
	if err != nil {
		t.Fatal(err)
	}
	originalBackup := backup + "-original"
	if err := os.Rename(backup, originalBackup); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "sentinel"), []byte("do not change"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, backup); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, key); err == nil {
		t.Fatal("startup followed a substituted restore backup symlink")
	}
	data, err := os.ReadFile(filepath.Join(outside, "sentinel"))
	if err != nil || string(data) != "do not change" {
		t.Fatal("restore recovery modified an unrelated directory")
	}
}

func TestRevisionRestoreJournalBlocksOfflineReindex(t *testing.T) {
	root, key, _, _, _ := revisionRestoreFixture(t)
	revisionRestoreKill(t, root, key, "journal")
	if err := sqliteReindexNoWAL(root); err == nil {
		t.Fatal("SQLite reindex accepted pending physical version replacement")
	}
}

func TestRevisionRestoreRecoversInterruptedCleanupInStages(t *testing.T) {
	for _, tc := range []struct {
		phase string
		folder string
	}{
		{phase: "journal", folder: "stage"},
		{phase: "new-moved", folder: "live"},
		{phase: "metadata", folder: "backup"},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			root, key, job, _, live := revisionRestoreFixture(t)
			revisionRestoreKill(t, root, key, tc.phase)
			raw, err := os.ReadFile(filepath.Join(root, revisionRestoreJournalName))
			if err != nil {
				t.Fatal(err)
			}
			var j revisionRestoreJournal
			if err := json.Unmarshal(raw, &j); err != nil {
				t.Fatal(err)
			}
			_, stage, backup, err := revisionRestorePaths(root, j)
			if err != nil {
				t.Fatal(err)
			}
			dir := map[string]string{"stage": stage, "live": live, "backup": backup}[tc.folder]
			// Simulate a second SIGKILL after cleanup has removed the
			// manifest, but before it has removed the directory inode.
			if err := os.Remove(filepath.Join(dir, "manifest.json")); err != nil {
				t.Fatal(err)
			}
			s, err := newStorage(root, key)
			if err != nil {
				t.Fatalf("second crash during cleanup blocked recovery: %v", err)
			}
			want := "Current"
			if tc.phase == "metadata" {
				want = "Previous"
			}
			if s.jobs[job.ID] == nil || s.jobs[job.ID].Title != want {
				t.Fatalf("wrong metadata after partial cleanup: %+v", s.jobs[job.ID])
			}
			if _, err := os.Lstat(filepath.Join(root, revisionRestoreJournalName)); !os.IsNotExist(err) {
				t.Fatal("recovery left durable journal after partial cleanup")
			}
		})
	}
}

func TestRevisionRestoreRefusesReplacedDirectoryWithMatchingManifest(t *testing.T) {
	root, key, _, _, _ := revisionRestoreFixture(t)
	revisionRestoreKill(t, root, key, "old-moved")
	raw, err := os.ReadFile(filepath.Join(root, revisionRestoreJournalName))
	if err != nil {
		t.Fatal(err)
	}
	var journal revisionRestoreJournal
	if err := json.Unmarshal(raw, &journal); err != nil {
		t.Fatal(err)
	}
	_, stage, _, err := revisionRestorePaths(root, journal)
	if err != nil {
		t.Fatal(err)
	}
	backup := stage + ".moved"
	if err := os.Rename(stage, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(backup, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), contents, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, key); err == nil {
		t.Fatal("same manifest but different directory inode was accepted")
	}
	if _, err := os.Stat(filepath.Join(backup, "index.html")); err != nil {
		t.Fatal("recovery modified original files after inode substitution", err)
	}
}

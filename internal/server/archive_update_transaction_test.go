package server

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	archiveUpdateCrashRoot  = "BOOSTY_ARCHIVE_UPDATE_CRASH_ROOT"
	archiveUpdateCrashKey   = "BOOSTY_ARCHIVE_UPDATE_CRASH_KEY"
	archiveUpdateCrashPhase = "BOOSTY_ARCHIVE_UPDATE_CRASH_PHASE"
)

func archiveUpdateFixture(t *testing.T, old bool) (string, string, Job, string) {
	t.Helper()
	root, key := t.TempDir(), testKey(t)
	s, err := newStorage(root, key)
	if err != nil {
		t.Fatal(err)
	}
	job := Job{ID: strings.Repeat("b", 24), OwnerID: "admin", Author: "creator",
		PostID: "post-a", State: "done", Title: "Old", SharedForAll: true,
		SharedWith: []string{"member"}, AutoSync: true}
	if err := s.upsert(job); err != nil {
		t.Fatal(err)
	}
	live := s.archiveDir(job.OwnerID, job.Author, job.PostID)
	if old {
		if err := os.MkdirAll(filepath.Join(live, "media"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(live, "media", "photo.jpg"), []byte("old media bytes"), 0600); err != nil {
			t.Fatal(err)
		}
		oldMediaSize, oldMediaHash, err := fileDigest(filepath.Join(live, "media", "photo.jpg"))
		if err != nil {
			t.Fatal(err)
		}
		man := ArchiveManifest{Title: "Old", Author: job.Author, PostID: job.PostID,
			CapturedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
			Assets:     []ArchiveAsset{{Filename: "photo.jpg", Type: "image", Size: oldMediaSize, SHA256: oldMediaHash}}}
		if err := writeJSON(filepath.Join(live, "manifest.json"), man); err != nil {
			t.Fatal(err)
		}
		if err := writeAtomic(filepath.Join(live, "index.html"), []byte("old archive HTML")); err != nil {
			t.Fatal(err)
		}
	}
	return root, key, job, live
}

func archiveUpdateWriteStage(t *testing.T, s *Storage, job Job) archiveUpdateStage {
	t.Helper()
	stage, err := s.beginArchiveUpdate(job)
	if err != nil {
		t.Fatal(err)
	}
	media := filepath.Join(stage.Path, "media")
	if err := os.MkdirAll(media, 0700); err != nil {
		t.Fatal(err)
	}
	newMedia := filepath.Join(media, "photo.jpg")
	if err := writeAtomic(newMedia, []byte("new media bytes")); err != nil {
		t.Fatal(err)
	}
	size, sha, err := fileDigest(newMedia)
	if err != nil {
		t.Fatal(err)
	}
	man := ArchiveManifest{Title: "New", Author: job.Author, PostID: job.PostID,
		CapturedAt: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC),
		Assets:     []ArchiveAsset{{Filename: "photo.jpg", Type: "image", Size: size, SHA256: sha}}}
	if err := writeJSON(filepath.Join(stage.Path, "manifest.json"), man); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(stage.Path, "index.html"), []byte("new archive HTML")); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(stage.Path, "post.json"), []byte(`{"data":[]}`)); err != nil {
		t.Fatal(err)
	}
	// Even if a CDN left partial bytes, never publish them as valid media.
	if err := os.WriteFile(filepath.Join(media, "photo.jpg.part"), []byte("unfinished"), 0600); err != nil {
		t.Fatal(err)
	}
	return stage
}

func TestArchiveUpdateRealSIGKILLWorker(t *testing.T) {
	root := os.Getenv(archiveUpdateCrashRoot)
	if root == "" {
		return
	}
	s, err := newStorage(root, os.Getenv(archiveUpdateCrashKey))
	if err != nil {
		t.Fatal(err)
	}
	job, ok := s.get(strings.Repeat("b", 24))
	if !ok {
		t.Fatal("missing synthetic job")
	}
	stage := archiveUpdateWriteStage(t, s, job)
	s.archiveUpdateCheckpoint = func(phase string) error {
		if phase == os.Getenv(archiveUpdateCrashPhase) {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	_, err = s.publishArchiveUpdate(job, stage)
	t.Fatalf("archive journal checkpoint was not reached: %v", err)
}

func archiveUpdateKill(t *testing.T, root, key, phase string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestArchiveUpdateRealSIGKILLWorker$")
	cmd.Env = append(os.Environ(), archiveUpdateCrashRoot+"="+root,
		archiveUpdateCrashKey+"="+key, archiveUpdateCrashPhase+"="+phase)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("worker was not killed at %s: %v %s", phase, err, output)
	}
	wait, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !wait.Signaled() || wait.Signal() != syscall.SIGKILL {
		t.Fatalf("unexpected crash worker exit at %s: %v %s", phase, err, output)
	}
}

func TestArchiveUpdateRecoversRealSIGKILL(t *testing.T) {
	for _, old := range []bool{false, true} {
		label := "new-post"
		if old {
			label = "existing-post"
		}
		t.Run(label, func(t *testing.T) {
			for _, phase := range []string{"journal", "old-moved", "new-moved", "metadata", "committed"} {
				t.Run(phase, func(t *testing.T) {
					root, key, job, live := archiveUpdateFixture(t, old)
					archiveUpdateKill(t, root, key, phase)
					if _, err := os.Lstat(filepath.Join(root, archiveUpdateJournalName)); err != nil {
						t.Fatalf("crash checkpoint failed to persist journal: %v", err)
					}
					recovered, err := newStorage(root, key)
					if err != nil {
						t.Fatalf("startup journal recovery failed: %v", err)
					}
					if recovered.jobs[job.ID] == nil || recovered.jobs[job.ID].OwnerID != job.OwnerID {
						t.Fatal("recovery changed owner or removed job")
					}
					if _, err := os.Lstat(filepath.Join(root, archiveUpdateJournalName)); !os.IsNotExist(err) {
						t.Fatal("archive update journal was not finalized")
					}
					expectedNew := phase == "metadata" || phase == "committed"
					actual := recovered.jobs[job.ID]
					if !actual.SharedForAll || !actual.AutoSync || len(actual.SharedWith) != 1 || actual.SharedWith[0] != "member" {
						t.Fatal("archive publication recovery changed sharing or autosync")
					}
					if expectedNew {
						if actual.Title != "New" || actual.State != "done" || actual.DoneFiles != 1 ||
							actual.TotalFiles != 1 || !actual.ArchiveUpdatedAt.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
							t.Fatalf("new directory and job metadata differ: %+v", actual)
						}
					} else if actual.Title != "Old" || !actual.ArchiveUpdatedAt.IsZero() {
						t.Fatalf("rollback overwrote old job metadata: %+v", actual)
					}
					if !expectedNew && !old {
						if _, err := os.Lstat(live); !os.IsNotExist(err) {
							t.Fatalf("uncommitted first archive became visible: %v", err)
						}
					} else {
						html, err := os.ReadFile(filepath.Join(live, "index.html"))
						want := "old archive HTML"
						if expectedNew {
							want = "new archive HTML"
						}
						if err != nil || string(html) != want {
							t.Fatalf("wrong published archive after %s: %q %v", phase, html, err)
						}
						media, err := os.ReadFile(filepath.Join(live, "media", "photo.jpg"))
						wantMedia := "old media bytes"
						if expectedNew {
							wantMedia = "new media bytes"
						}
						if err != nil || string(media) != wantMedia {
							t.Fatalf("wrong media after %s: %q %v", phase, media, err)
						}
						_, man, err := readArchiveManifestNoFollow(live)
						wantTitle := "Old"
						if expectedNew {
							wantTitle = "New"
						}
						if err != nil || man.Title != wantTitle {
							t.Fatalf("wrong manifest after %s: %+v %v", phase, man, err)
						}
						if _, err := os.Lstat(filepath.Join(live, "media", "photo.jpg.part")); !os.IsNotExist(err) {
							t.Fatal("partially downloaded media leaked into published archive")
						}
					}
					entries, err := os.ReadDir(filepath.Dir(live))
					if err != nil {
						t.Fatal(err)
					}
					for _, entry := range entries {
						if strings.HasPrefix(entry.Name(), archiveUpdateStagePrefix) ||
							strings.HasPrefix(entry.Name(), archiveUpdateBackupPrefix) {
							t.Fatalf("crash recovery leaked archive directory: %s", entry.Name())
						}
					}
				})
			}
		})
	}
}

func TestArchiveUpdateRollsBackOrdinaryFailure(t *testing.T) {
	root, key, job, live := archiveUpdateFixture(t, true)
	s, err := newStorage(root, key)
	if err != nil {
		t.Fatal(err)
	}
	stage := archiveUpdateWriteStage(t, s, job)
	s.archiveUpdateCheckpoint = func(phase string) error {
		if phase == "new-moved" {
			return syscall.ENOSPC
		}
		return nil
	}
	published, err := s.publishArchiveUpdate(job, stage)
	if err == nil || published {
		t.Fatalf("ENOSPC incorrectly committed archive: %v %v", published, err)
	}
	html, err := os.ReadFile(filepath.Join(live, "index.html"))
	if err != nil || string(html) != "old archive HTML" {
		t.Fatalf("failed publication overwrote live archive: %q %v", html, err)
	}
	if _, err := os.Stat(filepath.Join(root, archiveUpdateJournalName)); !os.IsNotExist(err) {
		t.Fatalf("failed archive publication retained journal: %v", err)
	}
}

func TestArchiveUpdateRefusesTamperedStage(t *testing.T) {
	root, key, _, live := archiveUpdateFixture(t, true)
	archiveUpdateKill(t, root, key, "journal")
	j, found, err := archiveUpdateReadJournal(root)
	if err != nil || !found {
		t.Fatal("no prepared journal", err)
	}
	stage := filepath.Join(filepath.Dir(live), archiveUpdateStagePrefix+j.Nonce)
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, key); err == nil {
		t.Fatal("startup trusted tampered archive update stage")
	}
	if _, err := os.Lstat(filepath.Join(root, archiveUpdateJournalName)); err != nil {
		t.Fatal("untrusted journal was deleted", err)
	}
}

func TestArchiveUpdateOfflineReindexBlocked(t *testing.T) {
	root, key, _, _ := archiveUpdateFixture(t, false)
	archiveUpdateKill(t, root, key, "journal")
	if err := sqliteReindexNoWAL(root); err == nil {
		t.Fatal("offline SQLite reindex accepted pending archive update")
	}
}

func TestArchiveUpdateRefusesUnknownJobsGeneration(t *testing.T) {
	root, key, _, live := archiveUpdateFixture(t, true)
	archiveUpdateKill(t, root, key, "new-moved")
	data, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(root, "jobs.json"), append(data, '\n')); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, key); err == nil {
		t.Fatal("startup guessed the result of a transaction with unknown jobs metadata")
	}
	if _, err := os.Lstat(filepath.Join(root, archiveUpdateJournalName)); err != nil {
		t.Fatal("unknown journal was removed", err)
	}
	if raw, err := os.ReadFile(filepath.Join(live, "index.html")); err != nil || string(raw) != "new archive HTML" {
		t.Fatalf("failed recovery modified live directory: %q %v", raw, err)
	}
}

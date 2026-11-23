package server

import (
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const videoJournalCrashRoot = "BOOSTY_VIDEO_JOURNAL_CRASH_ROOT"
const videoJournalCrashKey = "BOOSTY_VIDEO_JOURNAL_CRASH_KEY"
const videoJournalCrashStage = "BOOSTY_VIDEO_JOURNAL_CRASH_STAGE"
const videoJournalCrashPhase = "BOOSTY_VIDEO_JOURNAL_CRASH_PHASE"

func TestVideoPublicationSIGKILLWorker(t *testing.T) {
	root := os.Getenv(videoJournalCrashRoot)
	if root == "" {
		return
	}
	s, err := newStorage(root, os.Getenv(videoJournalCrashKey))
	if err != nil {
		t.Fatal(err)
	}
	job, ok := s.get(strings.Repeat("d", 24))
	if !ok {
		t.Fatal("crash worker cannot find the original archive")
	}
	folder := s.archiveDir(job.OwnerID, job.Author, job.PostID)
	_, manifest, err := readArchiveManifestNoFollow(folder)
	if err != nil || len(manifest.Assets) == 0 {
		t.Fatal("crash worker cannot read the original manifest", err)
	}
	staged := os.Getenv(videoJournalCrashStage)
	size, digest, err := fileDigest(staged)
	if err != nil {
		t.Fatal(err)
	}
	s.videoCheckpoint = func(phase string) error {
		if phase == os.Getenv(videoJournalCrashPhase) {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	}
	a := &App{store: s, archiveGates: s.gates}
	_, err = a.publishVideoRendition(job, manifest.CapturedAt, manifest.Assets[0],
		"movie.mp4.h264.mp4", staged, size, digest)
	t.Fatalf("video publication escaped required crash checkpoint: %v", err)
}

func videoJournalSIGKILL(t *testing.T, root, key, staged, phase string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestVideoPublicationSIGKILLWorker$")
	cmd.Env = append(os.Environ(),
		videoJournalCrashRoot+"="+root,
		videoJournalCrashKey+"="+key,
		videoJournalCrashStage+"="+staged,
		videoJournalCrashPhase+"="+phase,
	)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("video crash worker did not exit from SIGKILL: %v %s", err, output)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("video crash checkpoint did not send SIGKILL: %v %s", err, output)
	}
}

func TestVideoPublicationRecoversRealSIGKILL(t *testing.T) {
	for _, hadOld := range []bool{false, true} {
		name := "new-rendition"
		if hadOld {
			name = "replaced-rendition"
		}
		t.Run(name, func(t *testing.T) {
			for _, phase := range []string{"journal", "media", "manifest", "html", "committed"} {
				t.Run(phase, func(t *testing.T) {
					app, job, _, original, folder := videoPublicationFixture(t)
					root := app.store.root
					key := base64.StdEncoding.EncodeToString(app.store.key)
					target := "movie.mp4.h264.mp4"
					dest := filepath.Join(folder, "media", target)
					if hadOld {
						if err := os.WriteFile(dest, []byte("previous rendition"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					staged, _, _ := newVideoPublicationStage(t, root)
					videoJournalSIGKILL(t, root, key, staged, phase)
					if _, err := os.Lstat(filepath.Join(root, videoPublicationJournalName)); err != nil {
						t.Fatalf("SIGKILL did not preserve the publication journal: %v", err)
					}
					recovered, err := newStorage(root, key)
					if err != nil {
						t.Fatalf("unable to recover a verified video publication: %v", err)
					}
					if recovered.jobs[job.ID] == nil || recovered.jobs[job.ID].OwnerID != job.OwnerID {
						t.Fatal("journal recovery altered the owner")
					}
					if _, err := os.Lstat(filepath.Join(root, videoPublicationJournalName)); !os.IsNotExist(err) {
						t.Fatal("startup left an unresolved video publication journal")
					}
					_, man, err := readArchiveManifestNoFollow(folder)
					if err != nil {
						t.Fatal(err)
					}
					wantPublished := phase == "committed"
					if (len(man.Assets) == 2) != wantPublished {
						t.Fatalf("wrong video manifest after %s: %+v", phase, man.Assets)
					}
					if man.Assets[0].SHA256 != original.SHA256 {
						t.Fatal("video recovery modified original media metadata")
					}
					media, err := os.ReadFile(dest)
					switch {
					case wantPublished && (err != nil || string(media) != "encoded media bytes"):
						t.Fatalf("committed video not recovered: %q %v", media, err)
					case !wantPublished && hadOld && (err != nil || string(media) != "previous rendition"):
						t.Fatalf("original video rendition not restored: %q %v", media, err)
					case !wantPublished && !hadOld && !os.IsNotExist(err):
						t.Fatalf("rolled-back video rendition unexpectedly exists: %v", err)
					}
					html, err := os.ReadFile(filepath.Join(folder, "index.html"))
					if err != nil {
						t.Fatal(err)
					}
					if wantPublished && string(html) == "original HTML" {
						t.Fatal("committed transaction left obsolete HTML")
					}
					if !wantPublished && string(html) != "original HTML" {
						t.Fatal("uncommitted transaction modified HTML")
					}
					children, err := os.ReadDir(root)
					if err != nil {
						t.Fatal(err)
					}
					for _, child := range children {
						if strings.HasPrefix(child.Name(), videoPublicationStagePrefix) {
							t.Fatalf("recovered video transaction leaked staging directory: %s", child.Name())
						}
					}
				})
			}
		})
	}
}

func TestVideoPublicationRejectsDamagedJournalStage(t *testing.T) {
	app, _, _, _, folder := videoPublicationFixture(t)
	root := app.store.root
	key := base64.StdEncoding.EncodeToString(app.store.key)
	staged, _, _ := newVideoPublicationStage(t, root)
	videoJournalSIGKILL(t, root, key, staged, "media")
	j, found, err := videoPublicationReadJournal(root)
	if err != nil || !found {
		t.Fatal("missing video transaction journal", err)
	}
	stage := filepath.Join(root, videoPublicationStagePrefix+j.Nonce)
	if err := os.WriteFile(filepath.Join(stage, "manifest.before"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, key); err == nil {
		t.Fatal("startup accepted a tampered video publication stage")
	}
	if _, err := os.Lstat(filepath.Join(root, videoPublicationJournalName)); err != nil {
		t.Fatal("invalid recovery journal was erased", err)
	}
	if raw, err := os.ReadFile(filepath.Join(folder, "index.html")); err != nil || string(raw) != "original HTML" {
		t.Fatalf("invalid journal recovery changed original HTML: %q %v", raw, err)
	}
}

func TestVideoPublicationPendingJournalBlocksOfflineReindex(t *testing.T) {
	app, _, _, _, _ := videoPublicationFixture(t)
	root := app.store.root
	key := base64.StdEncoding.EncodeToString(app.store.key)
	staged, _, _ := newVideoPublicationStage(t, root)
	videoJournalSIGKILL(t, root, key, staged, "journal")
	if err := sqliteReindexNoWAL(root); err == nil {
		t.Fatal("offline SQLite reindex accepted an unresolved video publication journal")
	}
}

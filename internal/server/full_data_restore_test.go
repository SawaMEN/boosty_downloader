package server

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func fullRestoreTestPaths(t *testing.T) (string, string, string) {
	t.Helper()
	root, snapshot := fullSnapshotTestFixture(t)
	if err := PrepareFullDataSnapshot(root, snapshot); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "recovered-data")
	return root, snapshot, target
}

func fullRestoreCheckNoStage(t *testing.T, target string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".boosty-full-restore-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("failed restore left staging directories: %v (%v)", files, err)
	}
}

func TestFullDataRestoreRecoversArchivesOnlyIntoNewDirectory(t *testing.T) {
	root, snapshot, target := fullRestoreTestPaths(t)
	if err := RestoreFullDataSnapshot(snapshot, target); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		"jobs.json", "imports.json", "accounts.json", "authors.json",
		"settings/token.key",
		"library/user/author/post/index.html",
		"library/user/author/post/media/audio.bin",
		"revisions/user/author/post/version-1/manifest.json",
		"blobs/sha256/ab/abc",
	} {
		before, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			t.Fatal(err)
		}
		afterPath := filepath.Join(target, relative)
		after, err := os.ReadFile(afterPath)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("restored bytes differ for %s: %v", relative, err)
		}
		info, err := os.Stat(afterPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("restored file not private: %s (%v)", relative, err)
		}
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("restored directory not private: %v", err)
	}
	if err := RestoreFullDataSnapshot(snapshot, target); err == nil {
		t.Fatal("second restore overwrote existing destination")
	}
	fullRestoreCheckNoStage(t, target)
}

func TestFullDataRestoreRejectsUnsafeSnapshotOrDestination(t *testing.T) {
	t.Run("corrupt snapshot", func(t *testing.T) {
		_, snapshot, target := fullRestoreTestPaths(t)
		p := filepath.Join(snapshot, "data", "library", "user", "author", "post", "index.html")
		if err := os.WriteFile(p, []byte("tampered"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := RestoreFullDataSnapshot(snapshot, target); err == nil {
			t.Fatal("restored corrupted media snapshot")
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatal("corrupt snapshot published target")
		}
	})
	t.Run("occupied destination", func(t *testing.T) {
		_, snapshot, target := fullRestoreTestPaths(t)
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(target, "never-remove")
		if err := os.WriteFile(sentinel, []byte("untouched"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := RestoreFullDataSnapshot(snapshot, target); err == nil {
			t.Fatal("restore overwrote occupied directory")
		}
		if data, err := os.ReadFile(sentinel); err != nil || string(data) != "untouched" {
			t.Fatal("refused restore changed existing destination")
		}
	})
	t.Run("nested in snapshot", func(t *testing.T) {
		_, snapshot, _ := fullRestoreTestPaths(t)
		if err := RestoreFullDataSnapshot(snapshot, filepath.Join(snapshot, "recovered")); err == nil {
			t.Fatal("restore attempted to write inside verified snapshot")
		}
	})
	t.Run("symlinked destination", func(t *testing.T) {
		_, snapshot, target := fullRestoreTestPaths(t)
		other := t.TempDir()
		if err := os.Symlink(other, target); err != nil {
			t.Fatal(err)
		}
		if err := RestoreFullDataSnapshot(snapshot, target); err == nil {
			t.Fatal("restore followed symlink to existing destination")
		}
	})
}

func TestFullDataRestoreInjectedENOSPCLeavesNoTarget(t *testing.T) {
	_, snapshot, target := fullRestoreTestPaths(t)
	callCount := 0
	err := restoreFullDataSnapshotWithHooks(snapshot, target, &fullDataRestoreHooks{
		copyData: func(dst io.Writer, src io.Reader) (int64, error) {
			callCount++
			n, err := io.CopyN(dst, src, 5)
			if err != nil {
				return n, err
			}
			return n, syscall.ENOSPC
		},
	})
	if !errors.Is(err, syscall.ENOSPC) || callCount == 0 {
		t.Fatalf("partial copy did not report ENOSPC: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("failed staging published incomplete destination")
	}
	fullRestoreCheckNoStage(t, target)
	if err := RestoreFullDataSnapshot(snapshot, target); err != nil {
		t.Fatalf("failed restore was not retryable: %v", err)
	}
}

func TestFullDataRestoreRejectsDestinationCreatedBeforeAtomicPublish(t *testing.T) {
	_, snapshot, target := fullRestoreTestPaths(t)
	sentinel := []byte("external writer created this target")
	injected := false
	err := restoreFullDataSnapshotWithHooks(snapshot, target, &fullDataRestoreHooks{
		checkpoint: func(phase string) error {
			if phase != "verified" {
				return nil
			}
			injected = true
			if err := os.Mkdir(target, 0700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(target, "sentinel"), sentinel, 0600)
		},
	})
	if !injected || err == nil {
		t.Fatalf("restore replaced a destination created during staging: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(target, "sentinel"))
	if err != nil || !bytes.Equal(data, sentinel) {
		t.Fatal("no-overwrite atomic rename changed external writer's files")
	}
	fullRestoreCheckNoStage(t, target)
}

func TestFullDataRestoreRejectsSnapshotModifiedDuringCopy(t *testing.T) {
	_, snapshot, target := fullRestoreTestPaths(t)
	changed := false
	err := restoreFullDataSnapshotWithHooks(snapshot, target, &fullDataRestoreHooks{
		checkpoint: func(phase string) error {
			if phase != "copied" || changed {
				return nil
			}
			changed = true
			// accounts.json is the first copied root file; change it only
			// AFTER it was staged to exercise final source revalidation.
			file := filepath.Join(snapshot, "data", "accounts.json")
			return os.WriteFile(file, []byte("changed after copy"), 0600)
		},
	})
	if !changed || err == nil || !strings.Contains(err.Error(), "snapshot changed") {
		t.Fatalf("restore accepted changed snapshot source: %v", err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("moving source published restored data")
	}
	fullRestoreCheckNoStage(t, target)
}

// A crash must never expose an incomplete data directory at the final path.
// The kernel must terminate a helper process with SIGKILL, not a mock error.
func TestFullDataRestoreSIGKILLHelper(t *testing.T) {
	snapshot := os.Getenv("BD_TEST_FULL_RESTORE_SNAPSHOT")
	target := os.Getenv("BD_TEST_FULL_RESTORE_DEST")
	if snapshot == "" || target == "" {
		return
	}
	err := restoreFullDataSnapshotWithHooks(snapshot, target, &fullDataRestoreHooks{
		checkpoint: func(phase string) error {
			if phase == "copied" {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				os.Exit(111)
			}
			return nil
		},
	})
	t.Fatalf("restore helper was not SIGKILLed: %v", err)
}

func TestFullDataRestoreSIGKILLBeforePromotion(t *testing.T) {
	_, snapshot, target := fullRestoreTestPaths(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestFullDataRestoreSIGKILLHelper$")
	cmd.Env = append(os.Environ(),
		"BD_TEST_FULL_RESTORE_SNAPSHOT="+snapshot,
		"BD_TEST_FULL_RESTORE_DEST="+target)
	output, err := cmd.CombinedOutput()
	var failure *exec.ExitError
	if !errors.As(err, &failure) {
		t.Fatalf("helper did not exit after SIGKILL: %v (%s)", err, output)
	}
	status, ok := failure.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("helper exited without real SIGKILL: %v (%s)", err, output)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("SIGKILL exposed partial restore at destination")
	}
	orphans, err := filepath.Glob(filepath.Join(filepath.Dir(target), ".boosty-full-restore-*"))
	if err != nil || len(orphans) != 1 {
		t.Fatalf("expected one private staging directory from SIGKILL: %v (%v)", orphans, err)
	}
	info, err := os.Lstat(orphans[0])
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("SIGKILL staging directory is not private")
	}
	if err := RestoreFullDataSnapshot(snapshot, target); err != nil {
		t.Fatalf("restore could not resume after SIGKILL: %v", err)
	}
	if err := os.RemoveAll(orphans[0]); err != nil {
		t.Fatal(err)
	}
	fullRestoreCheckNoStage(t, target)
}

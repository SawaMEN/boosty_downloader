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

// Only the test binary can activate these hooks. The production CLI has no
// switch or environment variable that can trigger a premature process exit.
func TestSQLiteShadowRestoreCrashHelper(t *testing.T) {
	phase := os.Getenv("BD_TEST_SHADOW_RESTORE_CRASH_PHASE")
	if phase == "" {
		return
	}
	err := restoreSQLiteShadowWithHooks(
		os.Getenv("BD_TEST_SHADOW_RESTORE_ROOT"),
		os.Getenv("BD_TEST_SHADOW_RESTORE_BACKUP"),
		&restoreSQLiteShadowHooks{
			copyData: func(dst io.Writer, src io.Reader) (int64, error) {
				if phase != "copying" {
					return io.Copy(dst, src)
				}
				n, err := io.CopyN(dst, src, 256)
				if err != nil {
					return n, err
				}
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				os.Exit(111)
				return n, errors.New("SIGKILL unexpectedly returned")
			},
			checkpoint: func(at string) error {
				if at == phase {
					_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
					os.Exit(111)
				}
				return nil
			},
		},
	)
	t.Fatalf("restore helper returned without being killed at %s: %v", phase, err)
}

func sqliteRestoreReadTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func sqliteRestoreCheckUnchangedJSON(t *testing.T, root string, expected map[string][]byte) {
	t.Helper()
	for name, before := range expected {
		after := sqliteRestoreReadTestFile(t, filepath.Join(root, name))
		if !bytes.Equal(before, after) {
			t.Fatalf("restore crash or failure modified authoritative %s", name)
		}
	}
}

func sqliteRestoreCheckNoCurrentTemps(t *testing.T, root string) {
	t.Helper()
	for _, pattern := range []string{".sqlite-shadow-restoring-*", ".sqlite-shadow-previous-*"} {
		items, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 0 {
			t.Fatalf("failed non-crashing restore left temporary metadata files: %v", items)
		}
	}
}

func TestSQLiteShadowRestorePartialCopyENOSPCPreservesOriginal(t *testing.T) {
	root, backup, _, _, _ := sqliteReindexFixture(t)
	dbPath := filepath.Join(root, "metadata.db")
	oldSQL := sqliteRestoreReadTestFile(t, dbPath)
	expected := map[string][]byte{
		"jobs.json":    sqliteRestoreReadTestFile(t, filepath.Join(root, "jobs.json")),
		"imports.json": sqliteRestoreReadTestFile(t, filepath.Join(root, "imports.json")),
	}
	err := restoreSQLiteShadowWithHooks(root, backup, &restoreSQLiteShadowHooks{
		copyData: func(dst io.Writer, src io.Reader) (int64, error) {
			copied, err := io.CopyN(dst, src, 128)
			if err != nil {
				return copied, err
			}
			return copied, syscall.ENOSPC
		},
	})
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("injected disk-full error was not returned: %v", err)
	}
	if !bytes.Equal(oldSQL, sqliteRestoreReadTestFile(t, dbPath)) {
		t.Fatal("partial SQL copy replaced or changed the live database")
	}
	sqliteRestoreCheckUnchangedJSON(t, root, expected)
	sqliteRestoreCheckNoCurrentTemps(t, root)
	if err := VerifySQLiteCutoverPreflight(root, backup); err != nil {
		t.Fatalf("failed staging damaged the original SQL: %v", err)
	}
}

func TestSQLiteShadowRestoreENOSPCAfterRenameRollsBack(t *testing.T) {
	root, backup, _, _, _ := sqliteReindexFixture(t)
	dbPath := filepath.Join(root, "metadata.db")
	oldSQL := []byte("corrupted original SQL before offline recovery")
	if err := os.WriteFile(dbPath, oldSQL, 0600); err != nil {
		t.Fatal(err)
	}
	err := restoreSQLiteShadowWithHooks(root, backup, &restoreSQLiteShadowHooks{
		checkpoint: func(phase string) error {
			if phase == "promoted" {
				return syscall.ENOSPC
			}
			return nil
		},
	})
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("post-rename quota error was not propagated: %v", err)
	}
	if !bytes.Equal(sqliteRestoreReadTestFile(t, dbPath), oldSQL) {
		t.Fatal("failed post-rename verification did not restore original SQL")
	}
	sqliteRestoreCheckNoCurrentTemps(t, root)
	if err := RestoreSQLiteShadowFromBackup(root, backup); err != nil {
		t.Fatalf("restore was not retryable after an injected disk-full error: %v", err)
	}
	if err := VerifySQLiteCutoverPreflight(root, backup); err != nil {
		t.Fatalf("SQL restore retry did not recover a consistent snapshot: %v", err)
	}
}

func TestSQLiteShadowRestoreSIGKILLIsCrashConsistent(t *testing.T) {
	for _, phase := range []string{"copying", "staged", "promoted"} {
		t.Run(phase, func(t *testing.T) {
			root, backup, _, _, _ := sqliteReindexFixture(t)
			dbPath := filepath.Join(root, "metadata.db")
			backupSQL := sqliteRestoreReadTestFile(t, filepath.Join(backup, "metadata.db"))
			corruptSQL := []byte("broken live SQLite before offline recovery")
			if err := os.WriteFile(dbPath, corruptSQL, 0600); err != nil {
				t.Fatal(err)
			}
			before := map[string][]byte{
				"jobs.json":    sqliteRestoreReadTestFile(t, filepath.Join(root, "jobs.json")),
				"imports.json": sqliteRestoreReadTestFile(t, filepath.Join(root, "imports.json")),
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSQLiteShadowRestoreCrashHelper$")
			cmd.Env = append(os.Environ(),
				"BD_TEST_SHADOW_RESTORE_CRASH_PHASE="+phase,
				"BD_TEST_SHADOW_RESTORE_ROOT="+root,
				"BD_TEST_SHADOW_RESTORE_BACKUP="+backup)
			output, err := cmd.CombinedOutput()
			var killed *exec.ExitError
			if !errors.As(err, &killed) {
				t.Fatalf("child was not killed with SIGKILL: %v; output: %s", err, output)
			}
			status, ok := killed.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child exited without real SIGKILL: %v; output: %s", err, output)
			}
			sqliteRestoreCheckUnchangedJSON(t, root, before)
			got := sqliteRestoreReadTestFile(t, dbPath)
			if phase != "promoted" && !bytes.Equal(got, corruptSQL) {
				t.Fatal("SIGKILL during SQLite copy/staging published a partial SQLite file")
			}
			if phase == "copying" {
				partial, err := filepath.Glob(filepath.Join(root, ".sqlite-shadow-restoring-*"))
				if err != nil || len(partial) != 1 {
					t.Fatalf("expected one interrupted staging file: %v (%v)", partial, err)
				}
				info, err := os.Stat(partial[0])
				if err != nil || info.Size() != 256 {
					t.Fatalf("SIGKILL did not interrupt real temp write at 256 bytes: %v", err)
				}
			}
			if phase == "promoted" {
				if !bytes.Equal(got, backupSQL) {
					t.Fatal("SIGKILL after promotion left a partial SQLite file")
				}
				if err := VerifySQLiteCutoverPreflight(root, backup); err != nil {
					t.Fatalf("promoted recovery point was inconsistent after SIGKILL: %v", err)
				}
			}
			// The directory flock must be released by the kernel after an
			// uncatchable SIGKILL. Recovery must remain safe to retry.
			if err := RestoreSQLiteShadowFromBackup(root, backup); err != nil {
				t.Fatalf("offline recovery cannot resume after SIGKILL: %v", err)
			}
			if err := VerifySQLiteCutoverPreflight(root, backup); err != nil {
				t.Fatalf("retry after SIGKILL did not recover SQL/JSON/media: %v", err)
			}
			sqliteRestoreCheckNoCurrentTemps(t, root)
		})
	}
}

func TestSQLiteShadowRestoreRejectsBackupWithinLiveDirectory(t *testing.T) {
	root, _, _, _, _ := sqliteReindexFixture(t)
	nested := filepath.Join(root, "nested-recovery")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSQLiteShadowFromBackup(root, nested); err == nil ||
		!strings.Contains(err.Error(), "outside") {
		t.Fatalf("accepted backup under live data directory: %v", err)
	}
}

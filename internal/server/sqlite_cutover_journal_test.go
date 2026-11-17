package server

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func sqliteCutoverFixture(t *testing.T) (string, string, map[string][]byte) {
	t.Helper()
	root, backup, _, _, _ := sqliteReindexFixture(t)
	before := make(map[string][]byte)
	for _, name := range []string{"jobs.json", "imports.json", "metadata.db"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = raw
	}
	return root, backup, before
}

func sqliteCutoverCompare(t *testing.T, root string, before map[string][]byte) {
	t.Helper()
	for name, expected := range before {
		current, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || !bytes.Equal(current, expected) {
			t.Fatalf("cutover rehearsal changed authoritative %s: %v", name, err)
		}
	}
}

func sqliteCutoverApp(t *testing.T, root string) *App {
	t.Helper()
	t.Setenv("BD_SQLITE_SHADOW", "")
	app, err := New(Config{
		DataDir: root, SecretKey: testKey(t),
		AdminUser: "admin", AdminPassword: "strong-password-123",
	})
	if err != nil {
		t.Fatalf("startup rejected verified prepared cutover: %v", err)
	}
	t.Cleanup(app.Close)
	return app
}

func TestSQLiteCutoverRehearsalStartupAutoCancelsVerifiedIntent(t *testing.T) {
	root, backup, before := sqliteCutoverFixture(t)
	if err := PrepareSQLiteCutoverRehearsal(root, backup); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(root, sqliteCutoverIntentName))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		t.Fatalf("intent was not durably written as a private regular file: %v", err)
	}
	if err := PrepareSQLiteCutoverRehearsal(root, backup); err == nil {
		t.Fatal("an existing prepared journal was silently replaced")
	}
	sqliteCutoverCompare(t, root, before)
	app := sqliteCutoverApp(t, root)
	if _, err := os.Stat(filepath.Join(root, sqliteCutoverIntentName)); !os.IsNotExist(err) {
		t.Fatalf("startup left a verified uncommitted cutover intent: %v", err)
	}
	sqliteCutoverCompare(t, root, before)
	if _, ok := app.store.get(strings.Repeat("1", 24)); !ok {
		t.Fatal("startup failed to load the still-authoritative JSON archive")
	}
}

func TestSQLiteCutoverRehearsalFailsClosedOnChangedLiveJSON(t *testing.T) {
	root, backup, before := sqliteCutoverFixture(t)
	if err := PrepareSQLiteCutoverRehearsal(root, backup); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "jobs.json")
	if err := os.WriteFile(path, append(append([]byte(nil), before["jobs.json"]...), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{DataDir: root, SecretKey: testKey(t),
		AdminUser: "admin", AdminPassword: "strong-password-123"}
	if app, err := New(cfg); err == nil {
		app.Close()
		t.Fatal("server silently started despite changed JSON after cutover preparation")
	}
	if _, err := os.Lstat(filepath.Join(root, sqliteCutoverIntentName)); err != nil {
		t.Fatal("failed startup deleted an unresolved cutover journal:", err)
	}
	if err := RecoverSQLiteCutoverRehearsal(root); err == nil {
		t.Fatal("explicit recovery silently discarded an intent with different JSON")
	}
	if err := os.WriteFile(path, before["jobs.json"], 0600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverSQLiteCutoverRehearsal(root); err != nil {
		t.Fatalf("verified matching metadata was not recoverable: %v", err)
	}
	sqliteCutoverApp(t, root)
	sqliteCutoverCompare(t, root, before)
}

func TestSQLiteCutoverRehearsalRejectsTamperedSnapshot(t *testing.T) {
	root, backup, _ := sqliteCutoverFixture(t)
	if err := PrepareSQLiteCutoverRehearsal(root, backup); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(backup, "backup-manifest.json")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverSQLiteCutoverRehearsal(root); err == nil {
		t.Fatal("modified backup manifest was trusted during recovery")
	}
	if _, err := os.Lstat(filepath.Join(root, sqliteCutoverIntentName)); err != nil {
		t.Fatal("tampered backup caused journal to be discarded")
	}
	if err := os.WriteFile(manifest, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverSQLiteCutoverRehearsal(root); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteCutoverRehearsalRejectsUnsafeJournalAndNestedBackup(t *testing.T) {
	root, backup, _ := sqliteCutoverFixture(t)
	nested := filepath.Join(root, "recovery")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	if err := PrepareSQLiteCutoverRehearsal(root, nested); err == nil {
		t.Fatal("backup nested inside the live data directory was accepted")
	}
	if err := PrepareSQLiteCutoverRehearsal(root, backup); err != nil {
		t.Fatal(err)
	}
	intent := filepath.Join(root, sqliteCutoverIntentName)
	if err := os.Remove(intent); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(backup, "backup-manifest.json"), intent); err != nil {
		t.Fatal(err)
	}
	if err := RecoverSQLiteCutoverRehearsal(root); err == nil {
		t.Fatal("symlinked journal was accepted")
	}
	if err := os.Remove(intent); err != nil {
		t.Fatal(err)
	}
	if err := RecoverSQLiteCutoverRehearsal(root); err != nil {
		t.Fatal("no-op recovery with absent journal failed:", err)
	}
}

func TestSQLiteCutoverRehearsalENOSPCAndRetry(t *testing.T) {
	root, backup, before := sqliteCutoverFixture(t)
	for _, phase := range []string{"validated", "prepared"} {
		t.Run(phase, func(t *testing.T) {
			err := prepareSQLiteCutoverRehearsal(root, backup, &sqliteCutoverRehearsalHooks{
				checkpoint: func(at string) error {
					if at == phase {
						return syscall.ENOSPC
					}
					return nil
				},
			})
			if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("injected ENOSPC was lost: %v", err)
			}
			intent := filepath.Join(root, sqliteCutoverIntentName)
			_, statErr := os.Lstat(intent)
			if phase == "validated" && !os.IsNotExist(statErr) {
				t.Fatalf("ENOSPC before publishing left an intent: %v", statErr)
			}
			if phase == "prepared" && statErr != nil {
				t.Fatalf("ENOSPC after durable publication lost journal: %v", statErr)
			}
			if err := RecoverSQLiteCutoverRehearsal(root); err != nil {
				t.Fatalf("ENOSPC left unrecoverable state: %v", err)
			}
			sqliteCutoverCompare(t, root, before)
		})
	}
	if err := PrepareSQLiteCutoverRehearsal(root, backup); err != nil {
		t.Fatal("retry after ENOSPC failed:", err)
	}
	if err := RecoverSQLiteCutoverRehearsal(root); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteCutoverRehearsalRejectsActiveServer(t *testing.T) {
	root, backup, _ := sqliteCutoverFixture(t)
	app := sqliteCutoverApp(t, root)
	if err := PrepareSQLiteCutoverRehearsal(root, backup); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("live server allowed offline cutover: %v", err)
	}
	app.Close()
	if err := PrepareSQLiteCutoverRehearsal(root, backup); err != nil {
		t.Fatalf("shutdown did not release offline cutover lock: %v", err)
	}
	if err := RecoverSQLiteCutoverRehearsal(root); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteCutoverRehearsalSIGKILLHelper(t *testing.T) {
	phase := os.Getenv("BD_TEST_SQLITE_CUTOVER_KILL")
	if phase == "" {
		return
	}
	err := prepareSQLiteCutoverRehearsal(
		os.Getenv("BD_TEST_SQLITE_CUTOVER_ROOT"),
		os.Getenv("BD_TEST_SQLITE_CUTOVER_BACKUP"),
		&sqliteCutoverRehearsalHooks{checkpoint: func(at string) error {
			if at == phase {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				os.Exit(111)
			}
			return nil
		}},
	)
	t.Fatalf("child was not killed at %s: %v", phase, err)
}

func TestSQLiteCutoverRehearsalSIGKILLRecoversOnStartup(t *testing.T) {
	for _, phase := range []string{"validated", "prepared"} {
		t.Run(phase, func(t *testing.T) {
			root, backup, before := sqliteCutoverFixture(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestSQLiteCutoverRehearsalSIGKILLHelper$")
			cmd.Env = append(os.Environ(),
				"BD_TEST_SQLITE_CUTOVER_KILL="+phase,
				"BD_TEST_SQLITE_CUTOVER_ROOT="+root,
				"BD_TEST_SQLITE_CUTOVER_BACKUP="+backup)
			result, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("child was not killed by SIGKILL: %v %s", err, result)
			}
			ws, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
				t.Fatalf("child exited without SIGKILL: %v %s", err, result)
			}
			_, err = os.Lstat(filepath.Join(root, sqliteCutoverIntentName))
			if phase == "validated" && !os.IsNotExist(err) {
				t.Fatalf("SIGKILL before journal left a journal: %v", err)
			}
			if phase == "prepared" && err != nil {
				t.Fatalf("SIGKILL after journal lost the durable marker: %v", err)
			}
			sqliteCutoverApp(t, root)
			if _, err := os.Lstat(filepath.Join(root, sqliteCutoverIntentName)); !os.IsNotExist(err) {
				t.Fatalf("startup failed to clean a crash-recovered intent: %v", err)
			}
			sqliteCutoverCompare(t, root, before)
		})
	}
}

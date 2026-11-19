package server

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func detachedSQLRecoveryFixture(t *testing.T) (root, full, sqlExport, output, id string) {
	t.Helper()
	root, _, _, _, _ = sqliteReindexFixture(t)
	if err := os.MkdirAll(filepath.Join(root, "vault"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vault", "first.enc"), []byte("sensitive-token"), 0600); err != nil {
		t.Fatal(err)
	}
	full = filepath.Join(t.TempDir(), "full-data")
	if err := PrepareFullDataSnapshot(root, full); err != nil {
		t.Fatal(err)
	}
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	id = strings.Repeat("1", 24)
	original, err := meta.job(context.Background(), "first", id)
	if err != nil {
		t.Fatal(err)
	}
	updated := original
	updated.Title = "Newer SQL archive"
	updated.SharedWith = []string{"new-member"}
	updated.UpdatedAt = updated.UpdatedAt.Add(time.Minute)
	importID := strings.Repeat("a", 24)
	oldImport, err := meta.authorImport(context.Background(), "first", importID)
	if err != nil {
		t.Fatal(err)
	}
	newImport := oldImport
	newImport.State = "canceled"
	newImport.UpdatedAt = oldImport.UpdatedAt.Add(time.Minute)
	if err := meta.apply(context.Background(),
		[]sqliteArchiveChange{{Before: &original, After: updated}},
		[]sqliteImportChange{{Before: &oldImport, After: newImport}}); err != nil {
		t.Fatal(err)
	}
	if err := meta.close(); err != nil {
		t.Fatal(err)
	}
	sqlExport = filepath.Join(t.TempDir(), "export")
	if err := ExportSQLiteMetadataToJSON(root, sqlExport); err != nil {
		t.Fatal(err)
	}
	output = filepath.Join(t.TempDir(), "recovered")
	return root, full, sqlExport, output, id
}

func TestDetachedSQLRecoveryMergesNewSQLMetadataWithOldFullData(t *testing.T) {
	root, full, export, output, id := detachedSQLRecoveryFixture(t)
	before, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := RestoreDetachedSQLRecovery(full, export, output); err != nil {
		t.Fatal(err)
	}
	if err := verifySQLiteExport(output); err != nil {
		t.Fatal("restored SQL and JSON do not match:", err)
	}
	if err := verifySQLiteSnapshotInventory(output); err != nil {
		t.Fatal("restored media does not match SQL:", err)
	}
	jobs, err := sqliteShadowReadJobs(filepath.Join(output, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if jobs[id].Title != "Newer SQL archive" || len(jobs[id].SharedWith) != 1 ||
		jobs[id].SharedWith[0] != "new-member" {
		t.Fatalf("SQL-only job and sharing changes were lost: %+v", jobs[id])
	}
	imports, err := sqliteShadowReadImports(filepath.Join(output, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	if imports[strings.Repeat("a", 24)].State != "canceled" {
		t.Fatal("SQL-only import state was lost")
	}
	token, err := os.ReadFile(filepath.Join(output, "vault", "first.enc"))
	if err != nil || string(token) != "sensitive-token" {
		t.Fatal("full data backup did not restore encrypted user tokens")
	}
	original, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil || !bytes.Equal(before, original) {
		t.Fatal("live source JSON was changed by detached recovery")
	}
	if err := RestoreDetachedSQLRecovery(full, export, output); err == nil {
		t.Fatal("detached recovery overwrote an existing output")
	}
}

func TestDetachedSQLRecoveryRejectsChangedAccountIdentity(t *testing.T) {
	root, _, _, _, _ := sqliteReindexFixture(t)
	accounts := filepath.Join(root, "accounts.json")
	if err := os.WriteFile(accounts, []byte(`{"users":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(t.TempDir(), "full")
	if err := PrepareFullDataSnapshot(root, full); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(accounts, []byte(`{"users":{"later":{"role":"admin"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(t.TempDir(), "sql-export")
	if err := ExportSQLiteMetadataToJSON(root, export); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "recovered")
	if err := RestoreDetachedSQLRecovery(full, export, output); err == nil {
		t.Fatal("recovery accepted new SQL account identities against an old full snapshot")
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("unsafe account recovery published an output")
	}
}

func TestDetachedSQLRecoveryRejectsChangedOrMissingInputs(t *testing.T) {
	for _, scenario := range []string{"sql-manifest", "sql-payload", "full-snapshot", "output-inside-full", "output-inside-sql"} {
		t.Run(scenario, func(t *testing.T) {
			_, full, export, output, _ := detachedSQLRecoveryFixture(t)
			switch scenario {
			case "sql-manifest":
				if err := os.WriteFile(filepath.Join(export, "backup-manifest.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "sql-payload":
				if err := os.WriteFile(filepath.Join(export, "jobs.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "full-snapshot":
				if err := os.WriteFile(filepath.Join(full, "data", "jobs.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			case "output-inside-full":
				output = filepath.Join(full, "unsafe")
			case "output-inside-sql":
				output = filepath.Join(export, "unsafe")
			}
			if err := RestoreDetachedSQLRecovery(full, export, output); err == nil {
				t.Fatalf("unsafe recovery accepted scenario %s", scenario)
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatal("rejected recovery published a destination")
			}
		})
	}
}

func TestDetachedSQLRecoveryFailureAndDestinationRace(t *testing.T) {
	for _, scenario := range []string{"enospc", "source-changed", "destination-race"} {
		t.Run(scenario, func(t *testing.T) {
			_, full, export, output, _ := detachedSQLRecoveryFixture(t)
			hooks := &detachedSQLRestoreHooks{
				checkpoint: func(phase string) error {
					if scenario == "enospc" && phase == "overlaid" {
						return syscall.ENOSPC
					}
					if scenario == "source-changed" && phase == "overlaid" {
						return os.WriteFile(filepath.Join(export, "jobs.json"), []byte("{}"), 0600)
					}
					if scenario == "destination-race" && phase == "verified" {
						if err := os.Mkdir(output, 0700); err != nil {
							return err
						}
						return os.WriteFile(filepath.Join(output, "sentinel"), []byte("unchanged"), 0600)
					}
					return nil
				},
			}
			if err := restoreDetachedSQLRecoveryWithHooks(full, export, output, hooks); err == nil {
				t.Fatalf("%s did not prevent recovery", scenario)
			}
			if scenario == "destination-race" {
				content, err := os.ReadFile(filepath.Join(output, "sentinel"))
				if err != nil || string(content) != "unchanged" {
					t.Fatal("destination race replaced or corrupted a foreign directory")
				}
			} else if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatal("error published an unverified recovery")
			}
			remaining, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".boosty-sql-recovery-*"))
			if err != nil || len(remaining) != 0 {
				t.Fatalf("ordinary error left staging data: %v %v", remaining, err)
			}
		})
	}
}

func TestDetachedSQLRecoverySIGKILLHelper(t *testing.T) {
	if os.Getenv("BD_SQL_RECOVERY_KILL_CHILD") != "1" {
		return
	}
	full, export, output := os.Getenv("BD_SQL_RECOVERY_FULL"), os.Getenv("BD_SQL_RECOVERY_EXPORT"), os.Getenv("BD_SQL_RECOVERY_OUTPUT")
	err := restoreDetachedSQLRecoveryWithHooks(full, export, output, &detachedSQLRestoreHooks{
		checkpoint: func(phase string) error {
			if phase == "overlaid" {
				return syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
			}
			return nil
		},
	})
	t.Fatalf("SIGKILL recovery helper returned unexpectedly: %v", err)
}

func TestDetachedSQLRecoverySIGKILLBeforePublish(t *testing.T) {
	_, full, export, output, _ := detachedSQLRecoveryFixture(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestDetachedSQLRecoverySIGKILLHelper$")
	cmd.Env = append(os.Environ(),
		"BD_SQL_RECOVERY_KILL_CHILD=1",
		"BD_SQL_RECOVERY_FULL="+full,
		"BD_SQL_RECOVERY_EXPORT="+export,
		"BD_SQL_RECOVERY_OUTPUT="+output)
	err := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("expected SIGKILL, got: %v", err)
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("helper did not terminate by SIGKILL: %v", err)
	}
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		t.Fatal("SIGKILL exposed a partial recovered data directory")
	}
	if err := VerifyFullDataSnapshot(full); err != nil {
		t.Fatal("SIGKILL damaged source full snapshot:", err)
	}
	if err := VerifySQLiteMetadataBackup(export); err != nil {
		t.Fatal("SIGKILL damaged source SQL recovery export:", err)
	}
	if err := RestoreDetachedSQLRecovery(full, export, output); err != nil {
		t.Fatal("recovery did not succeed after a killed intermediate attempt:", err)
	}
}

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
	"time"
)

func sqliteRollbackFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root, _, _, _, _ := sqliteReindexFixture(t)
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := strings.Repeat("1", 24)
	old, err := meta.job(ctx, "first", id)
	if err != nil {
		t.Fatal(err)
	}
	next := old
	next.Title = "new SQL title"
	next.SharedWith = []string{"newmember"}
	next.UpdatedAt = next.UpdatedAt.Add(time.Minute)
	importID := strings.Repeat("a", 24)
	oldImport, err := meta.authorImport(ctx, "first", importID)
	if err != nil {
		t.Fatal(err)
	}
	nextImport := oldImport
	nextImport.State = "canceled"
	nextImport.UpdatedAt = nextImport.UpdatedAt.Add(time.Minute)
	if err := meta.apply(ctx, []sqliteArchiveChange{{Before: &old, After: next}},
		[]sqliteImportChange{{Before: &oldImport, After: nextImport}}); err != nil {
		t.Fatal(err)
	}
	if err := meta.close(); err != nil {
		t.Fatal(err)
	}
	if err := writeSyncedMetadata(root, sqliteBackendSelectionName, []byte(`{"version":1,"active":"sqlite"}`)); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(t.TempDir(), "full")
	if err := PrepareFullDataSnapshot(root, full); err != nil {
		t.Fatal(err)
	}
	sqlHash, _, err := backupFileDigest(root, "metadata.db")
	if err != nil {
		t.Fatal(err)
	}
	return root, full, sqlHash
}

func assertSQLiteRollback(t *testing.T, root, sqlHash string) {
	t.Helper()
	mode, err := readSQLiteBackendSelection(root)
	if err != nil || mode != "json" {
		t.Fatal("JSON not selected", mode, err)
	}
	jobs, err := sqliteShadowReadJobs(filepath.Join(root, "jobs.json"))
	job := jobs[strings.Repeat("1", 24)]
	if err != nil || job.Title != "new SQL title" || len(job.SharedWith) != 1 || job.SharedWith[0] != "newmember" {
		t.Fatal("new SQL metadata or ACL lost", job, err)
	}
	imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
	if err != nil || imports[strings.Repeat("a", 24)].State != "canceled" {
		t.Fatal("new SQL import lost", err)
	}
	actual, _, err := backupFileDigest(root, "metadata.retired-"+sqlHash+".db")
	if _, err := os.Lstat(filepath.Join(root, "metadata.db")); !os.IsNotExist(err) {
		t.Fatal("old SQLite remains active", err)
	}
	if err != nil || actual != sqlHash {
		t.Fatal("rollback changed SQL bytes", err)
	}
	if err := verifyJSONSQLiteAgreement(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, sqliteRollbackJournalName)); !os.IsNotExist(err) {
		t.Fatal("journal not cleaned")
	}
}

func TestSQLiteRollbackPreservesNewSQLWrites(t *testing.T) {
	root, full, hash := sqliteRollbackFixture(t)
	if err := RollbackSQLiteToJSON(root, full); err != nil {
		t.Fatal(err)
	}
	assertSQLiteRollback(t, root, hash)
	if err := VerifyFullDataSnapshot(full); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteRollbackSIGKILLWorker(t *testing.T) {
	root := os.Getenv("BOOSTY_TEST_ROLLBACK_ROOT")
	if root == "" {
		return
	}
	err := rollbackSQLiteToJSON(root, os.Getenv("BOOSTY_TEST_ROLLBACK_FULL"), func(phase string) error {
		if phase == os.Getenv("BOOSTY_TEST_ROLLBACK_PHASE") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	t.Fatalf("rollback worker escaped crash checkpoint: %v", err)
}

func TestSQLiteRollbackRecoversSIGKILL(t *testing.T) {
	for _, phase := range []string{"prepared", "jobs", "imports", "selector", "sidecars", "retired"} {
		t.Run(phase, func(t *testing.T) {
			root, full, hash := sqliteRollbackFixture(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestSQLiteRollbackSIGKILLWorker$")
			cmd.Env = append(os.Environ(), "BOOSTY_TEST_ROLLBACK_ROOT="+root, "BOOSTY_TEST_ROLLBACK_FULL="+full,
				"BOOSTY_TEST_ROLLBACK_PHASE="+phase)
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
				t.Fatal("offline tools ignored pending rollback")
			}
			if err := RecoverSQLiteRollback(root); err != nil {
				t.Fatal(err)
			}
			assertSQLiteRollback(t, root, hash)
		})
	}
}

func TestSQLiteRollbackRefusesUnknownJSONGeneration(t *testing.T) {
	root, full, _ := sqliteRollbackFixture(t)
	if err := rollbackSQLiteToJSON(root, full, func(phase string) error {
		if phase == "prepared" {
			return syscall.ENOSPC
		}
		return nil
	}); !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("expected staged error", err)
	}
	if err := writeSyncedMetadata(root, "imports.json", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := RecoverSQLiteRollback(root); err == nil {
		t.Fatal("overwrote unknown imports generation")
	}
	if mode, err := readSQLiteBackendSelection(root); err != nil || mode != "sqlite" {
		t.Fatal("invalid rollback selected JSON", mode, err)
	}
	if _, err := os.Lstat(filepath.Join(root, sqliteRollbackJournalName)); err != nil {
		t.Fatal("unresolved journal lost", err)
	}
}

func TestSQLiteRollbackRejectsStaleSnapshot(t *testing.T) {
	root, full, _ := sqliteRollbackFixture(t)
	if err := os.WriteFile(filepath.Join(root, "new-token"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RollbackSQLiteToJSON(root, full); err == nil {
		t.Fatal("accepted stale full snapshot")
	}
	if _, err := os.Lstat(filepath.Join(root, sqliteRollbackJournalName)); !os.IsNotExist(err) {
		t.Fatal("failed preflight left journal")
	}
}

func TestSQLiteRollbackRecoveryRejectsUnrelatedPrivateDataChange(t *testing.T) {
	root, full, _ := sqliteRollbackFixture(t)
	if err := rollbackSQLiteToJSON(root, full, func(phase string) error {
		if phase == "jobs" {
			return syscall.ENOSPC
		}
		return nil
	}); !errors.Is(err, syscall.ENOSPC) {
		t.Fatal("stage rollback", err)
	}
	extra := filepath.Join(root, "private-token-new")
	if err := os.WriteFile(extra, []byte("new private token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverSQLiteRollback(root); err == nil {
		t.Fatal("recovery accepted changed private data")
	}
	if _, err := os.Stat(filepath.Join(root, sqliteRollbackJournalName)); err != nil {
		t.Fatal("journal lost", err)
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	if err := RecoverSQLiteRollback(root); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteRollbackRoundTripJSONWritesSurviveRestart(t *testing.T) {
	cfg, backup, snapshot := sqlActivationFixture(t)
	if err := activateSQLPrimaryOffline(cfg.DataDir, backup, snapshot, nil); err != nil {
		t.Fatal(err)
	}
	app, err := newAppMode(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("d", 24)
	job, ok := app.store.get(id)
	if !ok {
		t.Fatal("missing archive")
	}
	job.Title = "new SQL generation"
	if err := app.store.upsert(job); err != nil {
		t.Fatal(err)
	}
	app.Close()
	full := filepath.Join(t.TempDir(), "rollback-full")
	if err := PrepareFullDataSnapshot(cfg.DataDir, full); err != nil {
		t.Fatal(err)
	}
	if err := RollbackSQLiteToJSON(cfg.DataDir, full); err != nil {
		t.Fatal(err)
	}
	app, err = New(cfg)
	if err != nil {
		t.Fatal("JSON restart after rollback", err)
	}
	job, ok = app.store.get(id)
	if !ok || job.Title != "new SQL generation" {
		t.Fatal("SQL write lost", job)
	}
	job.Title = "new JSON generation"
	if err := app.store.upsert(job); err != nil {
		t.Fatal(err)
	}
	app.Close()
	app, err = New(cfg)
	if err != nil {
		t.Fatal("second JSON restart after new writes", err)
	}
	defer app.Close()
	job, ok = app.store.get(id)
	if !ok || job.Title != "new JSON generation" {
		t.Fatal("new JSON write lost", job)
	}
	if app.store.primary != nil {
		t.Fatal("rollback silently opened SQL")
	}
}

func TestSQLiteRollbackCheckpointedSidecars(t *testing.T) {
	for _, scenario := range []string{"empty-wal", "nonempty-wal", "symlink-shm"} {
		t.Run(scenario, func(t *testing.T) {
			root, _, hash := sqliteRollbackFixture(t)
			wal := filepath.Join(root, "metadata.db-wal")
			shm := filepath.Join(root, "metadata.db-shm")
			if err := os.WriteFile(wal, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(shm, []byte("private checkpoint cache"), 0600); err != nil {
				t.Fatal(err)
			}
			full := filepath.Join(t.TempDir(), "snapshot")
			if err := PrepareFullDataSnapshot(root, full); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "nonempty-wal":
				if err := os.WriteFile(wal, []byte("uncheckpointed pages"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink-shm":
				if err := os.Remove(shm); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "jobs.json"), shm); err != nil {
					t.Fatal(err)
				}
			}
			err := RollbackSQLiteToJSON(root, full)
			if scenario != "empty-wal" {
				if err == nil {
					t.Fatal("unsafe rollback accepted")
				}
				if mode, e := readSQLiteBackendSelection(root); e != nil || mode != "sqlite" {
					t.Fatal("unsafe rollback changed selector", mode, e)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertSQLiteRollback(t, root, hash)
			for _, name := range []string{wal, shm} {
				if _, err := os.Lstat(name); !os.IsNotExist(err) {
					t.Fatal("stale sidecar", name, err)
				}
			}
		})
	}
}

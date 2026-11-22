package server

import (
	"bytes"
	"context"
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

func sqlPrimaryJournalFixture(t *testing.T) (string, map[string]Job, map[string]AuthorImport) {
	t.Helper()
	root, _ := sqliteCanaryFixture(t)
	jobs, err := sqliteShadowReadJobs(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	job := jobs[strings.Repeat("1", 24)]
	job.Title = "SQL primary change (newer)"
	job.SharedForAll = false
	job.SharedWith = []string{"new-member"}
	job.UpdatedAt = job.UpdatedAt.Add(time.Minute)
	jobs[job.ID] = job
	id := strings.Repeat("a", 24)
	imp := imports[id]
	imp.State = "canceled"
	imp.UpdatedAt = imp.UpdatedAt.Add(time.Minute)
	imports[id] = imp
	return root, jobs, imports
}

func sqlPrimaryAssertImages(t *testing.T, root string, after bool) {
	t.Helper()
	jobs, err := sqliteShadowReadJobs(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.close()
	if err := meta.verifyJSONParity(context.Background(), jobs, imports); err != nil {
		t.Fatal("JSON and SQLite diverged:", err)
	}
	job := jobs[strings.Repeat("1", 24)]
	imp := imports[strings.Repeat("a", 24)]
	if after {
		if job.Title != "SQL primary change (newer)" || len(job.SharedWith) != 1 ||
			job.SharedWith[0] != "new-member" || imp.State != "canceled" {
			t.Fatalf("SQL-primary forward recovery lost changes: job=%+v import=%+v", job, imp)
		}
	} else if job.Title == "SQL primary change (newer)" || imp.State == "canceled" {
		t.Fatal("SQL primary before-image was mutated without commit")
	}
	if _, err := os.Lstat(filepath.Join(root, sqlitePrimaryTransactionName)); !os.IsNotExist(err) {
		t.Fatal("SQL primary journal was not finalized:", err)
	}
}

func TestSQLPrimaryJournalAtomicCommitAndMirrors(t *testing.T) {
	root, jobs, imports := sqlPrimaryJournalFixture(t)
	lock, err := lockDataDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.close()
	if err := sqlitePrimaryCommitMetadata(root, meta, jobs, imports, nil); err != nil {
		t.Fatal(err)
	}
	sqlPrimaryAssertImages(t, root, true)
	if err := verifySQLiteExport(root); err != nil {
		t.Fatal("export cannot validate SQL-authoritative commit:", err)
	}
}

func TestSQLPrimaryJournalHandlesInjectedFailures(t *testing.T) {
	for _, tc := range []struct {
		checkpoint string
		after      bool
	}{
		{"prepared", false},
		{"sql-committed", true},
		{"json-committed", true},
	} {
		t.Run(tc.checkpoint, func(t *testing.T) {
			root, jobs, imports := sqlPrimaryJournalFixture(t)
			lock, err := lockDataDirectory(root)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer meta.close()
			if err := sqlitePrimaryCommitMetadata(root, meta, jobs, imports, &sqlitePrimaryCommitHooks{
				checkpoint: func(phase string) error {
					if phase == tc.checkpoint {
						return syscall.ENOSPC
					}
					return nil
				},
			}); !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("ENOSPC should report failure at %s, got %v", tc.checkpoint, err)
			}
			sqlPrimaryAssertImages(t, root, tc.after)
		})
	}
}

func TestSQLPrimaryJournalRejectsSQLProjectionDrift(t *testing.T) {
	root, jobs, imports := sqlPrimaryJournalFixture(t)
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.close()
	if _, err := meta.db.Exec("UPDATE owner_archives SET title='forged' WHERE archive_id=?", strings.Repeat("1", 24)); err != nil {
		t.Fatal(err)
	}
	if err := sqlitePrimaryCommitMetadata(root, meta, jobs, imports, nil); err == nil {
		t.Fatal("SQL-primary commit rewrote a forged SQL index")
	}
	if _, err := os.Lstat(filepath.Join(root, sqlitePrimaryTransactionName)); !os.IsNotExist(err) {
		t.Fatal("failed preflight persisted an SQL-primary journal")
	}
}

func TestSQLPrimaryJournalBadIntentBlocksRecovery(t *testing.T) {
	for _, scenario := range []string{"malformed", "unknown-version", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			root, _, _ := sqlPrimaryJournalFixture(t)
			path := filepath.Join(root, sqlitePrimaryTransactionName)
			payload := []byte("{")
			if scenario == "unknown-version" {
				payload, _ = json.Marshal(sqlitePrimaryTransaction{Version: 99})
			}
			if scenario == "symlink" {
				if err := os.Symlink(filepath.Join(root, "jobs.json"), path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, payload, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(root, "jobs.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := recoverSQLitePrimaryCommitLocked(root); err == nil {
				t.Fatal("malformed or unsafe SQL-primary journal was accepted")
			}
			after, err := os.ReadFile(filepath.Join(root, "jobs.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("unsafe SQL-primary recovery rewrote original JSON")
			}
		})
	}
}

func TestSQLPrimaryJournalSIGKILLHelper(t *testing.T) {
	if os.Getenv("BD_TEST_SQL_PRIMARY_KILL") != "1" {
		return
	}
	root := os.Getenv("BD_TEST_SQL_PRIMARY_ROOT")
	phase := os.Getenv("BD_TEST_SQL_PRIMARY_PHASE")
	jobs, err := sqliteShadowReadJobs(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	job := jobs[strings.Repeat("1", 24)]
	job.Title = "SQL primary change (newer)"
	job.SharedForAll = false
	job.SharedWith = []string{"new-member"}
	job.UpdatedAt = job.UpdatedAt.Add(time.Minute)
	jobs[job.ID] = job
	id := strings.Repeat("a", 24)
	imp := imports[id]
	imp.State = "canceled"
	imp.UpdatedAt = imp.UpdatedAt.Add(time.Minute)
	imports[id] = imp
	lock, err := lockDataDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.close()
	if err := sqlitePrimaryCommitMetadata(root, meta, jobs, imports, &sqlitePrimaryCommitHooks{
		checkpoint: func(current string) error {
			if current == phase {
				return syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	t.Fatal("SQL primary SIGKILL hook did not fire")
}

func TestSQLPrimaryJournalSIGKILLRecoversAcrossPhases(t *testing.T) {
	for _, tc := range []struct {
		phase string
		after bool
	}{
		{"prepared", false},
		{"sql-committed", true},
		{"json-committed", true},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			root, _, _ := sqlPrimaryJournalFixture(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestSQLPrimaryJournalSIGKILLHelper$")
			cmd.Env = append(os.Environ(),
				"BD_TEST_SQL_PRIMARY_KILL=1",
				"BD_TEST_SQL_PRIMARY_ROOT="+root,
				"BD_TEST_SQL_PRIMARY_PHASE="+tc.phase)
			err := cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("expected SIGKILL, got: %v", err)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("child did not terminate through SIGKILL: %v", err)
			}
			// Startup always first resolves the existing JSON transaction
			// (which may have been partially published) and then SQL.
			if err := recoverMetadataPair(root); err != nil {
				t.Fatal(err)
			}
			if err := recoverSQLitePrimaryCommitLocked(root); err != nil {
				t.Fatal("SQL crash recovery failed:", err)
			}
			sqlPrimaryAssertImages(t, root, tc.after)
		})
	}
}

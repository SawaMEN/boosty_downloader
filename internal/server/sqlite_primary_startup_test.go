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
)

func sqlStartupFixture(t *testing.T) (string, string) {
	t.Helper()
	root, key, _ := sqlRestoreFixture(t)
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := sqliteShadowReadJobs(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("1", 24)
	job := jobs[id]
	job.State = "running"
	job.Error = "interrupted"
	jobs[id] = job
	for id, imp := range imports {
		imp.State = "running"
		imports[id] = imp
	}
	if err := sqlitePrimaryCommitMetadata(root, meta, jobs, imports, nil); err != nil {
		t.Fatal(err)
	}
	if err := meta.close(); err != nil {
		t.Fatal(err)
	}
	if err := writeSyncedMetadata(root, sqliteBackendSelectionName, []byte(`{"version":1,"active":"sqlite"}`)); err != nil {
		t.Fatal(err)
	}
	return root, key
}

func TestSQLPrimaryStartupCrashWorker(t *testing.T) {
	root := os.Getenv("BOOSTY_TEST_SQL_STARTUP_ROOT")
	if root == "" {
		return
	}
	lock, err := lockDataDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	s, err := newStorageMode(root, os.Getenv("BOOSTY_TEST_SQL_STARTUP_KEY"), true, func(phase string) error {
		if phase == os.Getenv("BOOSTY_TEST_SQL_STARTUP_PHASE") {
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		return nil
	})
	if s != nil {
		_ = s.primary.close()
	}
	t.Fatalf("startup escaped crash checkpoint: %v", err)
}

func TestSQLPrimaryStartupRequeuesAfterSIGKILL(t *testing.T) {
	for _, phase := range []string{"prepared", "sql-committed", "json-committed"} {
		t.Run(phase, func(t *testing.T) {
			root, key := sqlStartupFixture(t)
			cmd := exec.Command(os.Args[0], "-test.run=^TestSQLPrimaryStartupCrashWorker$")
			cmd.Env = append(os.Environ(), "BOOSTY_TEST_SQL_STARTUP_ROOT="+root, "BOOSTY_TEST_SQL_STARTUP_KEY="+key, "BOOSTY_TEST_SQL_STARTUP_PHASE="+phase)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("worker did not crash: %v %s", err, out)
			}
			ws, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
				t.Fatalf("unexpected worker failure: %v %s", err, out)
			}
			lock, err := lockDataDirectory(root)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			s, err := newStorageMode(root, key, true, nil)
			if err != nil {
				t.Fatal("startup recovery:", err)
			}
			job, ok := s.get(strings.Repeat("1", 24))
			if !ok || job.State != "queued" || job.Error != "" || !job.SharedForAll || len(job.SharedWith) != 1 || !job.AutoSync {
				t.Fatal("requeue changed archive identity or ACL", job)
			}
			if len(s.queuedImports()) != 1 {
				t.Fatal("interrupted import not queued")
			}
			jobs, err := sqliteShadowReadJobs(filepath.Join(root, "jobs.json"))
			if err != nil {
				t.Fatal(err)
			}
			imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := s.primary.verifyJSONParity(context.Background(), jobs, imports); err != nil {
				t.Fatal(err)
			}
			if err := s.primary.close(); err != nil {
				t.Fatal(err)
			}
			// A clean second boot is read-only with respect to both mirrors.
			beforeJobs, err := os.ReadFile(filepath.Join(root, "jobs.json"))
			if err != nil {
				t.Fatal(err)
			}
			beforeImports, err := os.ReadFile(filepath.Join(root, "imports.json"))
			if err != nil {
				t.Fatal(err)
			}
			s, err = newStorageMode(root, key, true, nil)
			if err != nil {
				t.Fatal("second startup", err)
			}
			defer s.primary.close()
			afterJobs, err := os.ReadFile(filepath.Join(root, "jobs.json"))
			if err != nil {
				t.Fatal(err)
			}
			afterImports, err := os.ReadFile(filepath.Join(root, "imports.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeJobs, afterJobs) || !bytes.Equal(beforeImports, afterImports) {
				t.Fatal("idle startup rewrote mirrors")
			}
			if _, err := os.Stat(filepath.Join(root, sqlitePrimaryTransactionName)); !os.IsNotExist(err) {
				t.Fatal("journal leaked", err)
			}
		})
	}
}

func TestSQLPrimaryStartupRejectsMissingSelectorOrDivergentData(t *testing.T) {
	for _, scenario := range []string{"missing-selector", "missing-db", "SQL-drift", "media-drift"} {
		t.Run(scenario, func(t *testing.T) {
			root, key := sqlStartupFixture(t)
			switch scenario {
			case "missing-selector":
				if err := os.Remove(filepath.Join(root, sqliteBackendSelectionName)); err != nil {
					t.Fatal(err)
				}
			case "missing-db":
				if err := os.Remove(filepath.Join(root, "metadata.db")); err != nil {
					t.Fatal(err)
				}
			case "SQL-drift":
				meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := meta.db.Exec("UPDATE owner_archives SET title='forged' WHERE archive_id=?", strings.Repeat("1", 24)); err != nil {
					t.Fatal(err)
				}
				if err := meta.close(); err != nil {
					t.Fatal(err)
				}
			case "media-drift":
				if err := writeAtomic(filepath.Join(root, "library", "first", "creator", "post-1", "media", "photo.jpg"), []byte("forged")); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(filepath.Join(root, "jobs.json"))
			if err != nil {
				t.Fatal(err)
			}
			lock, err := lockDataDirectory(root)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if s, err := newStorageMode(root, key, true, nil); err == nil {
				_ = s.primary.close()
				t.Fatal("unsafe SQL startup accepted")
			}
			after, err := os.ReadFile(filepath.Join(root, "jobs.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("failed startup rewrote recovery mirror", err)
			}
			if scenario == "missing-db" {
				if _, err := os.Stat(filepath.Join(root, "metadata.db")); !os.IsNotExist(err) {
					t.Fatal("missing SQL database recreated", err)
				}
			}
		})
	}
}

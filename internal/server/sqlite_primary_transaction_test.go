package server

import (
	"bytes"
	"context"
	"database/sql"
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
		{"jobs-mirror", true},
		{"imports-mirror", true},
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
		{"jobs-mirror", true},
		{"imports-mirror", true},
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

func TestSQLPrimaryJournalSizeLimitBeforeCommit(t *testing.T) {
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
	err = sqlitePrimaryCommitMetadataWithLimit(root, meta, jobs, imports, &sqlitePrimaryCommitHooks{
		checkpoint: func(phase string) error {
			t.Fatalf("oversized journal reached checkpoint %s", phase)
			return nil
		},
	}, 1)
	if err == nil || !strings.Contains(err.Error(), "journal exceeds") {
		t.Fatalf("expected journal size rejection, got %v", err)
	}
	sqlPrimaryAssertImages(t, root, false)
	// A normal retry must remain possible without manual recovery.
	if err := sqlitePrimaryCommitMetadata(root, meta, jobs, imports, nil); err != nil {
		t.Fatal(err)
	}
	sqlPrimaryAssertImages(t, root, true)
}

func TestSQLPrimaryJournalEncodedSizeBoundary(t *testing.T) {
	journal := sqlitePrimaryTransaction{
		Version: sqlitePrimaryTransactionVersion,
		Jobs:    metadataImage{Before: []byte("{}"), After: []byte("{\"a\":1}")},
	}
	raw, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encodeSQLitePrimaryJournal(journal, len(raw)); err != nil {
		t.Fatalf("exact boundary rejected: %v", err)
	}
	if _, err := encodeSQLitePrimaryJournal(journal, len(raw)-1); err == nil {
		t.Fatal("oversized encoded journal accepted")
	}
}

func TestSQLPrimaryJournalUnsafeRecoveryInput(t *testing.T) {
	for _, scenario := range []string{"oversized", "public", "fifo", "replaced"} {
		t.Run(scenario, func(t *testing.T) {
			root, _, _ := sqlPrimaryJournalFixture(t)
			path := filepath.Join(root, sqlitePrimaryTransactionName)
			before, err := os.ReadFile(filepath.Join(root, "jobs.json"))
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "oversized":
				file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if err := file.Truncate(sqlitePrimaryJournalMaxBytes + 1); err != nil {
					file.Close()
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "replaced":
				if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := readSQLitePrimaryJournal(path, info); err == nil {
					t.Fatal("replaced journal accepted")
				}
			}
			if err := recoverSQLitePrimaryCommitLocked(root); err == nil {
				t.Fatal("unsafe journal accepted")
			}
			after, err := os.ReadFile(filepath.Join(root, "jobs.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("unsafe journal changed jobs mirror")
			}
		})
	}
}

func TestSQLPrimaryMirrorSkipsUnchangedFileAndNestedJournal(t *testing.T) {
	root, jobs, _ := sqlPrimaryJournalFixture(t)
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
	path := filepath.Join(root, "imports.json")
	imports, err := sqliteShadowReadImports(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(imports, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSyncedMetadata(root, "imports.json", raw); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitePrimaryCommitMetadata(root, meta, jobs, imports, &sqlitePrimaryCommitHooks{
		checkpoint: func(phase string) error {
			if _, err := os.Lstat(filepath.Join(root, metadataJournalName)); !os.IsNotExist(err) {
				t.Fatalf("unexpected nested JSON journal at %s: %v", phase, err)
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("unchanged import mirror was rewritten: %v", err)
	}
	if err := meta.verifyJSONParity(context.Background(), jobs, imports); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, sqlitePrimaryTransactionName)); !os.IsNotExist(err) {
		t.Fatalf("SQL journal not finalized: %v", err)
	}
}

func TestSQLPrimaryMirrorRejectsDivergentSecondTarget(t *testing.T) {
	root := t.TempDir()
	old, next := []byte("{}"), []byte("{\"new\":1}")
	for _, name := range []string{"jobs.json", "imports.json"} {
		if err := os.WriteFile(filepath.Join(root, name), old, 0600); err != nil {
			t.Fatal(err)
		}
	}
	img := metadataImage{Existed: true, Before: old, After: next, OldHash: metadataDigest(old), NewHash: metadataDigest(next)}
	journal := sqlitePrimaryTransaction{Version: sqlitePrimaryTransactionVersion, Jobs: img, Imports: img}
	if err := os.WriteFile(filepath.Join(root, "imports.json"), []byte("{\"foreign\":1}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishSQLitePrimaryMirrors(root, journal, nil); err == nil {
		t.Fatal("divergent second target accepted")
	}
	current, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil || !bytes.Equal(current, old) {
		t.Fatal("first mirror was modified before validating the second")
	}
}

// With no changed bytes and no extra effects, even a one-byte journal
// limit is sufficient: the operation must not create an intent.
func TestSQLPrimaryNoopCommitSkipsJournal(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
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
	jobs, err := sqliteShadowReadJobs(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Establish the exact canonical bytes, independent of fixture format.
	for _, item := range []struct {
		name  string
		value any
	}{
		{"jobs.json", jobs},
		{"imports.json", imports},
	} {
		raw, err := json.MarshalIndent(item.value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := writeSyncedMetadata(root, item.name, raw); err != nil {
			t.Fatal(err)
		}
	}
	beforeJobs, err := os.Stat(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	beforeImports, err := os.Stat(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlitePrimaryCommitMetadataWithLimit(root, meta, jobs, imports, nil, 1); err != nil {
		t.Fatal("no-op required a journal:", err)
	}
	for _, item := range []struct {
		name string
		info os.FileInfo
	}{
		{"jobs.json", beforeJobs},
		{"imports.json", beforeImports},
	} {
		after, err := os.Stat(filepath.Join(root, item.name))
		if err != nil || !os.SameFile(item.info, after) {
			t.Fatalf("no-op replaced %s: %v", item.name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, sqlitePrimaryTransactionName)); !os.IsNotExist(err) {
		t.Fatal("no-op left a journal:", err)
	}
	called := false
	if err := sqlitePrimaryCommitMetadata(root, meta, jobs, imports, &sqlitePrimaryCommitHooks{
		reconcileExtra: func(context.Context, *sql.Tx) error {
			called = true
			return nil
		},
	}); err != nil || !called {
		t.Fatalf("unchanged metadata skipped extra callback: called=%v err=%v", called, err)
	}
	// No-op is never permission to accept an unrelated forged SQL row.
	if _, err := meta.db.Exec("UPDATE owner_archives SET title='forged' WHERE archive_id=?", strings.Repeat("2", 24)); err != nil {
		t.Fatal(err)
	}
	if err := sqlitePrimaryCommitMetadataWithLimit(root, meta, jobs, imports, nil, 1); err == nil {
		t.Fatal("no-op accepted SQL drift")
	}
}

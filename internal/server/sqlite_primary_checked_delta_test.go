package server

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLPrimaryCheckedDeltaExtraRollback(t *testing.T) {
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
	injected := errors.New("extra failed")
	err = sqlitePrimaryCommitMetadata(root, meta, jobs, imports, &sqlitePrimaryCommitHooks{
		reconcileExtra: func(ctx context.Context, tx *sql.Tx) error {
			var title string
			if err := tx.QueryRowContext(ctx, "SELECT title FROM owner_archives WHERE archive_id=?", strings.Repeat("1", 24)).Scan(&title); err != nil {
				return err
			}
			if title != jobs[strings.Repeat("1", 24)].Title {
				t.Fatal("extra did not see updated SQL generation")
			}
			return injected
		},
	})
	if !errors.Is(err, injected) {
		t.Fatalf("expected injected failure: %v", err)
	}
	sqlPrimaryAssertImages(t, root, false)
	var count int
	if err := meta.db.QueryRow("SELECT COUNT(*) FROM archive_events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed transaction retained events: %d, %v", count, err)
	}
}

func TestSQLPrimaryCheckedDeltaRejectsUnchangedRowDrift(t *testing.T) {
	root, jobs, imports := sqlPrimaryJournalFixture(t)
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.close()
	id := strings.Repeat("a", 24)
	beforeImports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	imports[id] = beforeImports[id]
	if _, err := meta.db.Exec("UPDATE author_imports SET auto_sync=1-auto_sync WHERE import_id=?", id); err != nil {
		t.Fatal(err)
	}
	err = sqlitePrimaryCommitMetadata(root, meta, jobs, imports, &sqlitePrimaryCommitHooks{
		checkpoint: func(phase string) error {
			t.Fatalf("drift reached checkpoint %s", phase)
			return nil
		},
	})
	if err == nil {
		t.Fatal("drift in unchanged import was accepted")
	}
	if _, err := os.Lstat(filepath.Join(root, sqlitePrimaryTransactionName)); !os.IsNotExist(err) {
		t.Fatalf("drift created journal: %v", err)
	}
}

func TestSQLPrimaryCheckedDeltaNoopDoesNotWriteEvents(t *testing.T) {
	root, _, _ := sqlPrimaryJournalFixture(t)
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
	if err := sqlitePrimaryCommitMetadata(root, meta, jobs, imports, nil); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := meta.db.QueryRow("SELECT COUNT(*) FROM archive_events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("no-op produced events: %d, %v", count, err)
	}
}

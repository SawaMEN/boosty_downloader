package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteFullSnapshotRejectsDomainInvalidRows(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
	}{
		{"invalid-archive-state", "UPDATE owner_archives SET job_state='unrecognized', payload_json=json_set(payload_json,'$.state','unrecognized') WHERE archive_id='" + strings.Repeat("1", 24) + "'"},
		{"invalid-import-state", "UPDATE author_imports SET job_state='unrecognized', payload_json=json_set(payload_json,'$.state','unrecognized') WHERE import_id='" + strings.Repeat("a", 24) + "'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := sqliteCanaryFixture(t)
			meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer meta.close()
			if _, err := meta.db.Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			tx, err := meta.db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var readErr error
			if tc.name == "invalid-archive-state" {
				_, readErr = sqliteShadowSQLJobs(context.Background(), tx)
			} else {
				_, readErr = sqliteShadowSQLImports(context.Background(), tx)
			}
			if readErr == nil {
				t.Fatal("domain-invalid SQL row was accepted as a full metadata snapshot")
			}
		})
	}
}

func TestSQLiteFullSnapshotRejectsArchivesLostByJoin(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.close()
	// A corrupted foreign key can cause the INNER JOIN to silently omit
	// an archive. The full projection reader must detect the missing row.
	if _, err := meta.db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := meta.db.Exec("UPDATE owner_archives SET post_key=-998 WHERE archive_id=?", strings.Repeat("1", 24)); err != nil {
		t.Fatal(err)
	}
	tx, err := meta.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := sqliteShadowSQLJobs(context.Background(), tx); err == nil {
		t.Fatal("SQL snapshot silently dropped an orphan archive")
	}
}

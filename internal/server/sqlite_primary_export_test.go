package server

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetachedSQLToJSONRecoveryExportsNewerSQLWrites(t *testing.T) {
	root, _, _, _, _ := sqliteReindexFixture(t)
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := strings.Repeat("1", 24)
	job, err := meta.job(ctx, "first", id)
	if err != nil {
		t.Fatal(err)
	}
	oldJSON, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	after := job
	after.Title = "SQL has newer metadata"
	after.UpdatedAt = after.UpdatedAt.Add(time.Minute)
	after.SharedWith = []string{"newmember"}
	importID := strings.Repeat("a", 24)
	beforeImport, err := meta.authorImport(ctx, "first", importID)
	if err != nil {
		t.Fatal(err)
	}
	afterImport := beforeImport
	afterImport.State = "canceled"
	afterImport.UpdatedAt = afterImport.UpdatedAt.Add(time.Minute)
	if err := meta.apply(ctx, []sqliteArchiveChange{{Before: &job, After: after}},
		[]sqliteImportChange{{Before: &beforeImport, After: afterImport}}); err != nil {
		t.Fatal(err)
	}
	if err := meta.close(); err != nil {
		t.Fatal(err)
	}
	export := filepath.Join(t.TempDir(), "sql-json-recovery")
	if err := ExportSQLiteMetadataToJSON(root, export); err != nil {
		t.Fatal("valid newer SQL recovery export was rejected:", err)
	}
	if err := VerifySQLiteMetadataBackup(export); err != nil {
		t.Fatal("new detached SQL/JSON snapshot failed independent check:", err)
	}
	jobs, err := sqliteShadowReadJobs(filepath.Join(export, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if v := jobs[id]; v.Title != after.Title || len(v.SharedWith) != 1 ||
		v.SharedWith[0] != "newmember" {
		t.Fatalf("SQL-only job and grants were not exported: %+v", v)
	}
	imports, err := sqliteShadowReadImports(filepath.Join(export, "imports.json"))
	if err != nil || imports[importID].State != "canceled" {
		t.Fatalf("newer SQL import was not exported: %+v, %v", imports, err)
	}
	current, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil || string(current) != string(oldJSON) {
		t.Fatal("source JSON changed during a detached recovery export")
	}
	if err := ExportSQLiteMetadataToJSON(root, export); err == nil {
		t.Fatal("existing verified destination was overwritten")
	}
}

func TestDetachedSQLToJSONRecoveryRefusesUnsafeState(t *testing.T) {
	for _, tc := range []struct {
		name        string
		breakSource func(*testing.T, string)
	}{
		{"forged-grant", func(t *testing.T, root string) {
			db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			_, err = db.Exec("INSERT INTO archive_grants(archive_id,grantee_owner_id,created_at) VALUES(?,?,?)",
				strings.Repeat("1", 24), "forged", "2026-11-11T12:00:00Z")
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"forged-index", func(t *testing.T, root string) {
			db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			_, err = db.Exec("UPDATE owner_archives SET job_state='done' WHERE archive_id=?", strings.Repeat("1", 24))
			if err != nil {
				t.Fatal(err)
			}
		}},
		{"uncheckpointed-wal", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "metadata.db-wal"), []byte("pending"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"metadata-journal", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, metadataJournalName), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"primary-intent", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, sqlitePrimaryTransactionName), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"cutover-intent", func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, sqliteCutoverIntentName), []byte("{}"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _, _, _, _ := sqliteReindexFixture(t)
			tc.breakSource(t, root)
			dest := filepath.Join(t.TempDir(), "export")
			if err := ExportSQLiteMetadataToJSON(root, dest); err == nil {
				t.Fatal("unsafe SQL recovery export succeeded")
			}
			if _, err := os.Lstat(dest); !os.IsNotExist(err) {
				t.Fatal("failed export published an unsafe destination")
			}
		})
	}
}

func TestDetachedSQLToJSONRecoveryRequiresExclusiveLockAndExternalDestination(t *testing.T) {
	root, _, _, _, _ := sqliteReindexFixture(t)
	lock, err := lockDataDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := ExportSQLiteMetadataToJSON(root, filepath.Join(t.TempDir(), "export")); err == nil {
		t.Fatal("export succeeded with active server lock")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ExportSQLiteMetadataToJSON(root, filepath.Join(root, "export")); err == nil {
		t.Fatal("export wrote inside the application data directory")
	}
}

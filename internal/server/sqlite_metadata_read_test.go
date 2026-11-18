package server

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A future SQL-primary adapter must never serve an owner-qualified row whose
// indexed identity, status or ACL differs from the JSON payload in that row.
func TestSQLiteMetadataPointReadRejectsArchiveProjectionDrift(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	store, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	ctx := context.Background()
	id := strings.Repeat("1", 24)
	if _, err := store.job(ctx, "first", id); err != nil {
		t.Fatal("baseline SQL archive was rejected:", err)
	}
	if _, err := store.job(ctx, "second", id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-owner read was accepted: %v", err)
	}
	for _, tc := range []struct {
		name, corrupt, undo string
	}{
		{"status", "UPDATE owner_archives SET job_state='done' WHERE archive_id=?", "UPDATE owner_archives SET job_state='failed' WHERE archive_id=?"},
		{"title", "UPDATE owner_archives SET title='forged' WHERE archive_id=?", "UPDATE owner_archives SET title='' WHERE archive_id=?"},
		{"public-grant", "UPDATE owner_archives SET shared_for_all=1 WHERE archive_id=?", "UPDATE owner_archives SET shared_for_all=0 WHERE archive_id=?"},
		{"autosync", "UPDATE owner_archives SET auto_sync=1 WHERE archive_id=?", "UPDATE owner_archives SET auto_sync=0 WHERE archive_id=?"},
		{"timestamp", "UPDATE owner_archives SET updated_at='2001-01-01T00:00:00Z' WHERE archive_id=?", "UPDATE owner_archives SET updated_at='2026-11-11T12:00:00Z' WHERE archive_id=?"},
		{"payload", "UPDATE owner_archives SET payload_json='{}' WHERE archive_id=?", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var original string
			if tc.name == "payload" {
				if err := store.db.QueryRow("SELECT payload_json FROM owner_archives WHERE archive_id=?", id).Scan(&original); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.db.Exec(tc.corrupt, id); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if tc.name == "payload" {
					_, err = store.db.Exec("UPDATE owner_archives SET payload_json=? WHERE archive_id=?", original, id)
				} else {
					_, err = store.db.Exec(tc.undo, id)
				}
				if err != nil {
					t.Errorf("failed to restore SQL fixture: %v", err)
				}
			})
			if _, err := store.job(ctx, "first", id); err == nil {
				t.Fatal("inconsistent SQL archive projection was served")
			}
		})
		if _, err := store.job(ctx, "first", id); err != nil {
			t.Fatalf("valid SQL archive was not readable after restoring %s: %v", tc.name, err)
		}
	}
	if _, err := store.db.Exec("UPDATE source_posts SET author_slug='forged' WHERE author_slug='creator'"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.job(ctx, "first", id); err == nil {
		t.Fatal("changed source post identity was accepted")
	}
}

func TestSQLiteMetadataPointReadRejectsAccessGrantDrift(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	store, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	ctx := context.Background()
	id := strings.Repeat("1", 24)
	if _, err := store.db.Exec("DELETE FROM archive_grants WHERE archive_id=?", id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.job(ctx, "first", id); err == nil {
		t.Fatal("missing grant was ignored")
	}
	if _, err := store.db.Exec("INSERT INTO archive_grants (archive_id,grantee_owner_id,created_at) VALUES (?,?,?)",
		id, "second", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.job(ctx, "first", id); err != nil {
		t.Fatal("restored grants still rejected:", err)
	}
	if _, err := store.db.Exec("INSERT INTO archive_grants (archive_id,grantee_owner_id,created_at) VALUES (?,?,?)",
		id, "third", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.job(ctx, "first", id); err == nil {
		t.Fatal("forged extra access grant was ignored")
	}
}

func TestSQLiteMetadataPointReadRejectsImportProjectionDrift(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	store, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	ctx := context.Background()
	id := strings.Repeat("a", 24)
	if _, err := store.authorImport(ctx, "first", id); err != nil {
		t.Fatal("baseline SQL import was rejected:", err)
	}
	if _, err := store.authorImport(ctx, "second", id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-owner import read was accepted: %v", err)
	}
	for _, tc := range []struct {
		name, corrupt, undo string
	}{
		{"author", "UPDATE author_imports SET author_slug='forged' WHERE import_id=?", "UPDATE author_imports SET author_slug='creator' WHERE import_id=?"},
		{"state", "UPDATE author_imports SET job_state='running' WHERE import_id=?", "UPDATE author_imports SET job_state='done' WHERE import_id=?"},
		{"autosync", "UPDATE author_imports SET auto_sync=1 WHERE import_id=?", "UPDATE author_imports SET auto_sync=0 WHERE import_id=?"},
		{"timestamp", "UPDATE author_imports SET updated_at='2001-01-01T00:00:00Z' WHERE import_id=?", "UPDATE author_imports SET updated_at='2026-11-11T12:00:00Z' WHERE import_id=?"},
		{"payload", "UPDATE author_imports SET payload_json='{}' WHERE import_id=?", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var original string
			if tc.name == "payload" {
				if err := store.db.QueryRow("SELECT payload_json FROM author_imports WHERE import_id=?", id).Scan(&original); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.db.Exec(tc.corrupt, id); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if tc.name == "payload" {
					_, err = store.db.Exec("UPDATE author_imports SET payload_json=? WHERE import_id=?", original, id)
				} else {
					_, err = store.db.Exec(tc.undo, id)
				}
				if err != nil {
					t.Errorf("failed to restore SQL import fixture: %v", err)
				}
			})
			if _, err := store.authorImport(ctx, "first", id); err == nil {
				t.Fatal("inconsistent SQL import projection was served")
			}
		})
		if _, err := store.authorImport(ctx, "first", id); err != nil {
			t.Fatalf("valid SQL import not readable after restoring %s: %v", tc.name, err)
		}
	}
}

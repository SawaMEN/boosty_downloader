package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Optimistic CAS must reject a forged index or ACL even when the old
// payload_json matches Before. The entire SQL batch must remain unchanged.
func TestSQLiteArchiveCASRejectsSQLProjectionDrift(t *testing.T) {
	for _, tc := range []struct {
		name, statement string
	}{
		{"state", "UPDATE owner_archives SET job_state='done' WHERE archive_id=?"},
		{"title", "UPDATE owner_archives SET title='forged' WHERE archive_id=?"},
		{"public", "UPDATE owner_archives SET shared_for_all=1 WHERE archive_id=?"},
		{"autosync", "UPDATE owner_archives SET auto_sync=1 WHERE archive_id=?"},
		{"timestamp", "UPDATE owner_archives SET updated_at='2001-01-01T00:00:00Z' WHERE archive_id=?"},
		{"missing-grant", "DELETE FROM archive_grants WHERE archive_id=?"},
		{"extra-grant", "INSERT INTO archive_grants (archive_id,grantee_owner_id,created_at) VALUES (?, 'forged', '2026-11-11T12:00:00Z')"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := sqliteCanaryFixture(t)
			store, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.close()
			ctx := context.Background()
			id := strings.Repeat("1", 24)
			before, err := store.job(ctx, "first", id)
			if err != nil {
				t.Fatal(err)
			}
			after := before
			after.Title = "A valid update must not erase SQL drift"
			after.UpdatedAt = after.UpdatedAt.Add(time.Minute)
			if _, err := store.db.Exec(tc.statement, id); err != nil {
				t.Fatal(err)
			}
			if err := store.apply(ctx, []sqliteArchiveChange{{Before: &before, After: after}}, nil); !errors.Is(err, errSQLiteConflict) {
				t.Fatalf("forged SQL index accepted by archive CAS: %v", err)
			}
			var raw string
			if err := store.db.QueryRow("SELECT payload_json FROM owner_archives WHERE archive_id=?", id).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			if !sqliteSameJSON([]byte(raw), mustSQLiteJSON(t, before)) {
				t.Fatal("failed archive CAS changed the payload")
			}
			var events int
			if err := store.db.QueryRow("SELECT COUNT(*) FROM archive_events").Scan(&events); err != nil || events != 0 {
				t.Fatalf("failed CAS wrote audit events: %d, %v", events, err)
			}
		})
	}
}

func TestSQLiteImportCASRejectsSQLProjectionDrift(t *testing.T) {
	for _, tc := range []struct {
		name, statement string
	}{
		{"author", "UPDATE author_imports SET author_slug='forged' WHERE import_id=?"},
		{"state", "UPDATE author_imports SET job_state='running' WHERE import_id=?"},
		{"autosync", "UPDATE author_imports SET auto_sync=1 WHERE import_id=?"},
		{"timestamp", "UPDATE author_imports SET updated_at='2001-01-01T00:00:00Z' WHERE import_id=?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := sqliteCanaryFixture(t)
			store, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.close()
			ctx := context.Background()
			id := strings.Repeat("a", 24)
			before, err := store.authorImport(ctx, "first", id)
			if err != nil {
				t.Fatal(err)
			}
			after := before
			after.State = "canceled"
			after.UpdatedAt = after.UpdatedAt.Add(time.Minute)
			if _, err := store.db.Exec(tc.statement, id); err != nil {
				t.Fatal(err)
			}
			if err := store.apply(ctx, nil, []sqliteImportChange{{Before: &before, After: after}}); !errors.Is(err, errSQLiteConflict) {
				t.Fatalf("forged SQL index accepted by import CAS: %v", err)
			}
			var state string
			if err := store.db.QueryRow("SELECT job_state FROM author_imports WHERE import_id=?", id).Scan(&state); err != nil {
				t.Fatal(err)
			}
			if state == "canceled" {
				t.Fatal("failed import CAS applied the new state")
			}
		})
	}
}

func TestSQLiteArchiveCASTrashAndRestore(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	store, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	ctx := context.Background()
	id := strings.Repeat("1", 24)
	before, err := store.job(ctx, "first", id)
	if err != nil {
		t.Fatal(err)
	}
	deleted := before
	stamp := before.UpdatedAt.Add(time.Minute)
	deleted.DeletedAt = &stamp
	deleted.UpdatedAt = stamp
	deleted.SharedWith = nil
	deleted.SharedForAll = false
	deleted.AutoSync = false
	if err := store.apply(ctx, []sqliteArchiveChange{{Before: &before, After: deleted}}, nil); err != nil {
		t.Fatal("trash transition rejected:", err)
	}
	if _, err := store.job(ctx, "first", id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted job visible to normal point read: %v", err)
	}
	restored := deleted
	restored.DeletedAt = nil
	restored.UpdatedAt = stamp.Add(time.Minute)
	if err := store.apply(ctx, []sqliteArchiveChange{{Before: &deleted, After: restored}}, nil); err != nil {
		t.Fatal("restoring deleted job through checked CAS failed:", err)
	}
	if _, err := store.job(ctx, "first", id); err != nil {
		t.Fatal("restored job not readable:", err)
	}
}

func mustSQLiteJSON(t *testing.T, value any) []byte {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sqlFirstStorageFixture(t *testing.T) (*Storage, *sqliteMetadata, string) {
	t.Helper()
	root, _ := sqliteCanaryFixture(t)
	// Normal construction performs the current JSON boot migrations. Only
	// AFTER initialization may the internal SQL-first adapter be attached.
	store, err := newStorage(root, testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.close() })
	if err := verifySQLiteExport(root); err != nil {
		t.Fatal("test fixture must have identical SQL/JSON checkpoint:", err)
	}
	store.primary = meta
	return store, meta, root
}

func assertSQLFirstStorageParity(t *testing.T, meta *sqliteMetadata, root string) {
	t.Helper()
	jobs, err := sqliteShadowReadJobs(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := meta.verifyJSONParity(context.Background(), jobs, imports); err != nil {
		t.Fatal("SQL-primary adapter left SQL/JSON out of sync:", err)
	}
	if err := verifySQLiteExport(root); err != nil {
		t.Fatal("SQL-authoritative writes did not preserve independent SQLite export audit:", err)
	}
	if _, err := os.Lstat(filepath.Join(root, sqlitePrimaryTransactionName)); !os.IsNotExist(err) {
		t.Fatalf("SQL-primary adapter leaked an unfinished journal: %v", err)
	}
}

func TestSQLFirstStorageRoutesJobImportACLAndCombinedWrites(t *testing.T) {
	store, meta, root := sqlFirstStorageFixture(t)
	id := strings.Repeat("1", 24)
	job, ok := store.get(id)
	if !ok {
		t.Fatal("expected existing job")
	}
	job.Title = "Ready to share"
	job.State = "done"
	if err := store.upsert(job); err != nil {
		t.Fatal("job writer failed to commit to SQL:", err)
	}
	assertSQLFirstStorageParity(t, meta, root)

	if changed, err := store.changeShare(id, "first", "third", true); err != nil || !changed {
		t.Fatalf("sharing writer did not commit to SQL: changed=%v err=%v", changed, err)
	}
	shared, err := meta.job(context.Background(), "first", id)
	if err != nil || len(shared.SharedWith) != 2 || shared.SharedWith[1] != "third" {
		t.Fatalf("owner SQL ACL did not reflect grant: %+v %v", shared, err)
	}
	assertSQLFirstStorageParity(t, meta, root)

	now := time.Now().UTC()
	newImport := AuthorImport{
		ID: strings.Repeat("b", 24), OwnerID: "first",
		Author: "another", State: "queued", CreatedAt: now, UpdatedAt: now,
	}
	added, err := store.createImports([]AuthorImport{newImport})
	if err != nil || len(added) != 1 {
		t.Fatalf("import writer did not commit to SQL: %+v %v", added, err)
	}
	assertSQLFirstStorageParity(t, meta, root)
	if err := store.cancelImport("first", newImport.ID); err != nil {
		t.Fatal("cancellation did not commit to SQL:", err)
	}
	assertSQLFirstStorageParity(t, meta, root)

	if err := store.setImportSchedule("first", strings.Repeat("a", 24), true); err != nil {
		t.Fatal("combined jobs/imports scheduler did not commit atomically:", err)
	}
	assertSQLFirstStorageParity(t, meta, root)
	imported, err := meta.authorImport(context.Background(), "first", strings.Repeat("a", 24))
	if err != nil || !imported.AutoSync {
		t.Fatalf("combined import state missing in SQLite: %+v %v", imported, err)
	}
	shared, err = meta.job(context.Background(), "first", id)
	if err != nil || !shared.AutoSync {
		t.Fatalf("combined job state missing in SQLite: %+v %v", shared, err)
	}
}

func TestSQLFirstStorageDoesNotFallBackToJSONAfterBadSQLACL(t *testing.T) {
	store, meta, root := sqlFirstStorageFixture(t)
	id := strings.Repeat("1", 24)
	job, ok := store.get(id)
	if !ok {
		t.Fatal("missing expected archive")
	}
	before, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Forged SQL index prevents claiming that a JSON write was also
	// persisted in SQL. Never silently fall back to the old JSON writer.
	if _, err := meta.db.Exec("UPDATE owner_archives SET title='forged-index' WHERE archive_id=?", id); err != nil {
		t.Fatal(err)
	}
	job.Title = "must not be written"
	if err := store.upsert(job); err == nil {
		t.Fatal("SQL-first route silently fell back to JSON after corrupt SQL")
	}
	after, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("corrupt SQL allowed JSON-only write")
	}
	if store.primaryFatal == nil {
		t.Fatal("SQL-primary route failed to quarantine SQL/JSON divergence")
	}
	if err := store.upsert(job); err == nil {
		t.Fatal("quarantined SQL-primary route accepted a second write")
	}
}

func TestSQLFirstStorageRefusesConcurrentShadowWriter(t *testing.T) {
	store, _, root := sqlFirstStorageFixture(t)
	store.shadow = &sqliteShadow{}
	job, ok := store.get(strings.Repeat("1", 24))
	if !ok {
		t.Fatal("missing test archive")
	}
	before, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	job.Title = "must not persist"
	if err := store.upsert(job); err == nil {
		t.Fatal("SQL-primary adapter allowed a shadow reconciler to overwrite newer SQL")
	}
	after, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("shadow/SQL-primary conflict wrote JSON")
	}
}

func TestSQLFirstProductionFlagStillRejected(t *testing.T) {
	t.Setenv("BD_SQLITE_PRIMARY", "1")
	root, _ := sqliteCanaryFixture(t)
	if app, err := New(Config{
		DataDir: root, AdminUser: "admin",
		AdminPassword: "strong-password-123", SecretKey: testKey(t),
	}); err == nil {
		app.Close()
		t.Fatal("untested production SQL-primary mode was enabled")
	}
}

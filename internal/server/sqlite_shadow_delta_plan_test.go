package server

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteShadowStreamingDelta(t *testing.T) {
	root, jobs := sqliteCanaryFixture(t)
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.close()
	ctx := context.Background()
	imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := meta.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := sqlitePlanShadowDelta(ctx, tx, jobs, imports)
	_ = tx.Rollback()
	if err != nil || len(plan.archives)+len(plan.removeArchives)+len(plan.importChanges)+len(plan.removeImports) != 0 {
		t.Fatalf("unchanged inventory produced a delta: %+v %v", plan, err)
	}
	first, second := strings.Repeat("1", 24), strings.Repeat("2", 24)
	changed := jobs[first]
	changed.Title = "changed"
	changed.UpdatedAt = changed.UpdatedAt.Add(time.Minute)
	jobs[first] = changed
	delete(jobs, second)
	added := changed
	added.ID = strings.Repeat("3", 24)
	added.PostID = "new-post"
	added.SharedWith = nil
	jobs[added.ID] = added
	for id, item := range imports {
		item.AutoSync = !item.AutoSync
		item.UpdatedAt = item.UpdatedAt.Add(time.Minute)
		imports[id] = item
	}
	if err := meta.reconcile(ctx, jobs, imports); err != nil {
		t.Fatal(err)
	}
	if err := meta.verifyJSONParity(ctx, jobs, imports); err != nil {
		t.Fatal("streaming delta differs from desired inventory:", err)
	}
}

func TestSQLiteShadowStreamingDeltaRejectsLateDrift(t *testing.T) {
	root, jobs := sqliteCanaryFixture(t)
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.close()
	ctx := context.Background()
	imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("1", 24)
	changed := jobs[id]
	oldTitle := changed.Title
	changed.Title = "must not commit"
	jobs[id] = changed
	if _, err := meta.db.Exec("UPDATE author_imports SET job_state=CASE WHEN job_state='done' THEN 'failed' ELSE 'done' END"); err != nil {
		t.Fatal(err)
	}
	called := false
	err = meta.reconcileWithExtra(ctx, jobs, imports, func(context.Context, *sql.Tx) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatalf("invalid import did not abort reconciliation: %v", err)
	}
	var title string
	if err := meta.db.QueryRow("SELECT title FROM owner_archives WHERE archive_id=?", id).Scan(&title); err != nil || title != oldTitle {
		t.Fatalf("archive changed before complete validation: %q %v", title, err)
	}
}

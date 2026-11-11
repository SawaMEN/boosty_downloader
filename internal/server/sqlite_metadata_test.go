package server

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteMetadataAtomicArchiveImportAndGrants(t *testing.T) {
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
	if _, err := store.job(ctx, "second", id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("cross-owner SQL read was not denied: %v", err)
	}
	importID := strings.Repeat("a", 24)
	importBefore, err := store.authorImport(ctx, "first", importID)
	if err != nil {
		t.Fatal(err)
	}
	j := before
	j.Title = "Updated title"
	j.SharedWith = []string{"third"}
	j.SharedForAll = true
	j.UpdatedAt = j.UpdatedAt.Add(time.Minute)
	third := Job{
		ID: strings.Repeat("3", 24), OwnerID: "third", Author: "creator",
		PostID: "post-1", State: "queued", CreatedAt: j.CreatedAt,
		UpdatedAt: j.UpdatedAt, Title: "Third private snapshot",
	}
	imp := importBefore
	imp.State = "canceled"
	imp.UpdatedAt = imp.UpdatedAt.Add(time.Minute)
	if err := store.apply(ctx,
		[]sqliteArchiveChange{{Before: &before, After: j}, {After: third}},
		[]sqliteImportChange{{Before: &importBefore, After: imp}}); err != nil {
		t.Fatalf("transaction failed: %v", err)
	}
	updated, err := store.job(ctx, "first", id)
	if err != nil || updated.Title != "Updated title" || !updated.SharedForAll ||
		len(updated.SharedWith) != 1 || updated.SharedWith[0] != "third" {
		t.Fatalf("wrong archive after transaction: %+v, %v", updated, err)
	}
	if _, err := store.job(ctx, "third", third.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.job(ctx, "first", third.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("third owner's archive leaked: %v", err)
	}
	gotImp, err := store.authorImport(ctx, "first", importID)
	if err != nil || gotImp.State != "canceled" {
		t.Fatalf("import update not committed: %+v %v", gotImp, err)
	}
	var posts, owners, grants, events int
	for i, q := range []string{
		"SELECT COUNT(*) FROM source_posts",
		"SELECT COUNT(*) FROM owner_archives",
		"SELECT COUNT(*) FROM archive_grants",
		"SELECT COUNT(*) FROM archive_events",
	} {
		var dest *int
		switch i {
		case 0:
			dest = &posts
		case 1:
			dest = &owners
		case 2:
			dest = &grants
		default:
			dest = &events
		}
		if err := store.db.QueryRow(q).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	if posts != 1 || owners != 3 || grants != 1 || events != 2 {
		t.Fatalf("unexpected counts: posts=%d owners=%d grants=%d events=%d", posts, owners, grants, events)
	}
	var member string
	if err := store.db.QueryRow("SELECT grantee_owner_id FROM archive_grants WHERE archive_id=?", id).Scan(&member); err != nil || member != "third" {
		t.Fatalf("grant not changed atomically: %s %v", member, err)
	}
	// The offline source must remain authoritative until explicit cutover.
	if err := verifySQLiteExport(root); err == nil {
		t.Fatal("stale SQLite must be detected against unchanged JSON")
	}
}

func TestSQLiteMetadataOptimisticConflictAndFailedBatchRollback(t *testing.T) {
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
	first := before
	first.Title = "First write"
	first.UpdatedAt = first.UpdatedAt.Add(time.Minute)
	if err := store.apply(ctx, []sqliteArchiveChange{{Before: &before, After: first}}, nil); err != nil {
		t.Fatal(err)
	}
	second := before
	second.Title = "Stale write"
	if err := store.apply(ctx, []sqliteArchiveChange{{Before: &before, After: second}}, nil); !errors.Is(err, errSQLiteConflict) {
		t.Fatalf("expected optimistic conflict, got: %v", err)
	}
	current, err := store.job(ctx, "first", id)
	if err != nil || current.Title != "First write" {
		t.Fatalf("first write lost: %+v %v", current, err)
	}
	var events int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM archive_events").Scan(&events); err != nil || events != 1 {
		t.Fatalf("conflicting update incorrectly wrote an audit event: %d %v", events, err)
	}
	// Abort after the archive payload is updated but before its grant is
	// committed. The enclosing transaction MUST revert the archive as well.
	if _, err := store.db.Exec(`
		CREATE TRIGGER reject_grant BEFORE INSERT ON archive_grants
		WHEN NEW.grantee_owner_id='blocked'
		BEGIN SELECT RAISE(ABORT, 'simulated failed grant'); END`); err != nil {
		t.Fatal(err)
	}
	importID := strings.Repeat("a", 24)
	importBefore, err := store.authorImport(ctx, "first", importID)
	if err != nil {
		t.Fatal(err)
	}
	importAfter := importBefore
	importAfter.AutoSync = true
	broken := current
	broken.Title = "Must rollback"
	broken.SharedWith = []string{"blocked"}
	if err := store.apply(ctx,
		[]sqliteArchiveChange{{Before: &current, After: broken}},
		[]sqliteImportChange{{Before: &importBefore, After: importAfter}}); err == nil {
		t.Fatal("simulated SQL constraint failure unexpectedly committed")
	}
	current, err = store.job(ctx, "first", id)
	if err != nil || current.Title != "First write" || len(current.SharedWith) != 1 || current.SharedWith[0] != "second" {
		t.Fatalf("failed batch modified archive or grants: %+v %v", current, err)
	}
	gotImp, err := store.authorImport(ctx, "first", importID)
	if err != nil || gotImp.AutoSync {
		t.Fatalf("failed batch modified import: %+v %v", gotImp, err)
	}
}

func TestSQLiteMetadataRejectsUnsafeAndCrossOwnerMutations(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	path := filepath.Join(root, "metadata.db")
	store, err := openSQLiteMetadata(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	ctx := context.Background()
	before, err := store.job(ctx, "first", strings.Repeat("1", 24))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutated := range []Job{
		func() Job { j := before; j.OwnerID = "second"; return j }(),
		func() Job { j := before; j.Author = "other"; return j }(),
		func() Job { j := before; j.SharedWith = []string{".."}; return j }(),
		func() Job { j := before; j.SharedWith = []string{"second", "second"}; return j }(),
	} {
		if err := store.apply(ctx, []sqliteArchiveChange{{Before: &before, After: mutated}}, nil); err == nil {
			t.Fatalf("unsafe update accepted: %+v", mutated)
		}
	}
	if err := store.apply(ctx, []sqliteArchiveChange{{After: before}}, nil); !errors.Is(err, errSQLiteConflict) {
		t.Fatalf("duplicate job insert must fail: %v", err)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM archive_events").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected writes left events: %d %v", count, err)
	}
	if _, err := openSQLiteMetadata(filepath.Join(root, "missing.db")); !os.IsNotExist(err) {
		t.Fatalf("missing database was silently created: %v", err)
	}
	link := filepath.Join(root, "alias.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openSQLiteMetadata(link); err == nil {
		t.Fatal("symlink database accepted")
	}
}

func TestSQLiteMetadataTrashRevokesSharingAndHidesArchive(t *testing.T) {
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
	bad := before
	when := time.Now().UTC()
	bad.DeletedAt = &when
	if err := store.apply(ctx, []sqliteArchiveChange{{Before: &before, After: bad}}, nil); err == nil {
		t.Fatal("trash operation retaining shared grants accepted")
	}
	deleted := before
	deleted.DeletedAt = &when
	deleted.SharedWith = nil
	deleted.SharedForAll = false
	deleted.AutoSync = false
	deleted.UpdatedAt = when
	if err := store.apply(ctx, []sqliteArchiveChange{{Before: &before, After: deleted}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.job(ctx, "first", id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted archive remains readable: %v", err)
	}
	var grants, visible int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM archive_grants WHERE archive_id=?", id).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow("SELECT shared_for_all FROM owner_archives WHERE archive_id=?", id).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if grants != 0 || visible != 0 {
		t.Fatal("deleted archive retained sharing")
	}
	if _, err := store.job(ctx, "second", strings.Repeat("2", 24)); err != nil {
		t.Fatal("independent owner's archive became inaccessible:", err)
	}
}

func TestSQLiteMetadataImportFailureRollsBackEarlierArchiveWrite(t *testing.T) {
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
	previousImport, err := store.authorImport(ctx, "first", strings.Repeat("a", 24))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_import BEFORE UPDATE ON author_imports
		BEGIN SELECT RAISE(ABORT, 'simulated disk error'); END`); err != nil {
		t.Fatal(err)
	}
	after := before
	after.Title = "Must not be committed"
	nextImport := previousImport
	nextImport.State = "canceled"
	if err := store.apply(ctx, []sqliteArchiveChange{{Before: &before, After: after}},
		[]sqliteImportChange{{Before: &previousImport, After: nextImport}}); err == nil {
		t.Fatal("failed import unexpectedly committed transaction")
	}
	unchanged, err := store.job(ctx, "first", id)
	if err != nil || unchanged.Title != before.Title {
		t.Fatalf("failed import left partial archive update: %+v %v", unchanged, err)
	}
	var events int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM archive_events").Scan(&events); err != nil || events != 0 {
		t.Fatalf("partial event committed: %d %v", events, err)
	}
}

func TestSQLiteMetadataPreventsImmutableCreationTimeChanges(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	store, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.close()
	ctx := context.Background()
	before, err := store.job(ctx, "first", strings.Repeat("1", 24))
	if err != nil {
		t.Fatal(err)
	}
	changed := before
	changed.CreatedAt = changed.CreatedAt.Add(time.Hour)
	if err := store.apply(ctx, []sqliteArchiveChange{{Before: &before, After: changed}}, nil); err == nil {
		t.Fatal("archive creation date was rewritten")
	}
	imp, err := store.authorImport(ctx, "first", strings.Repeat("a", 24))
	if err != nil {
		t.Fatal(err)
	}
	next := imp
	next.CreatedAt = next.CreatedAt.Add(time.Hour)
	if err := store.apply(ctx, nil, []sqliteImportChange{{Before: &imp, After: next}}); err == nil {
		t.Fatal("import creation date was rewritten")
	}
}

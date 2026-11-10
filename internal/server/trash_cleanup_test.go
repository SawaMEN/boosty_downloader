package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func trashTestJob(t *testing.T, store *Storage, id string) Job {
	t.Helper()
	job := Job{ID: id, OwnerID: "admin", Author: "creator", PostID: id, State: "done"}
	if err := store.upsert(job); err != nil {
		t.Fatal(err)
	}
	dir := store.archiveDir(job.OwnerID, job.Author, job.PostID)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("archived"), 0600); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestTrashRetentionGuardAndRecovery(t *testing.T) {
	root := t.TempDir()
	store, err := newStorage(root, testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	old := trashTestJob(t, store, "aaaaaaaaaaaaaaaaaaaaaaaa")
	recent := trashTestJob(t, store, "bbbbbbbbbbbbbbbbbbbbbbbb")
	for _, job := range []Job{old, recent} {
		if n, err := store.removeOwned("admin", job.ID, ""); err != nil || n != 1 {
			t.Fatalf("trash %s: %d %v", job.ID, n, err)
		}
	}
	store.mu.Lock()
	older := time.Now().UTC().Add(-40 * 24 * time.Hour)
	store.jobs[old.ID].DeletedAt = &older
	if err := store.saveJobsLocked(); err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	store.mu.Unlock()
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	items, count := store.expiredTrash(cutoff, 20)
	if count != 1 || len(items) != 1 || items[0].id != old.ID {
		t.Fatalf("invalid cutoff selection %+v %d", items, count)
	}
	if _, err := store.restoreOwned("admin", old.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.removeOwned("admin", old.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.purgeOwnedBefore("admin", old.ID, "", cutoff); err == nil {
		t.Fatal("stale candidate deleted after restore and re-trash")
	}
	if _, err := os.Stat(filepath.Join(store.archiveDir(old.OwnerID, old.Author, old.PostID), "index.html")); err != nil {
		t.Fatal("archive bytes removed by stale sweep", err)
	}
	if _, err := store.purgeOwnedBefore("admin", recent.ID, "", cutoff); err == nil {
		t.Fatal("purged a recent archive")
	}
}

func TestTrashAdminSweepJournalAndPolicy(t *testing.T) {
	root := t.TempDir()
	cfg := Config{AdminUser: "admin", AdminPassword: "strong-password-123", DataDir: root, SecretKey: testKey(t)}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	admin := testLogin(t, app, "admin", "strong-password-123")
	member := testMember(t, app, "trashmember")
	initial := app.accounts.policy()
	if initial.Trash.AutoPurge || initial.Trash.RetentionDays != 30 {
		t.Fatalf("unsafe trash defaults: %+v", initial.Trash)
	}
	policyRequest(t, app, member, "GET", "/api/admin/trash", "", 403)
	policyRequest(t, app, member, "POST", "/api/admin/trash/cleanup", "", 403)
	policyRequest(t, app, admin, "GET", "/api/admin/trash", "", 200)
	job := trashTestJob(t, app.store, "cccccccccccccccccccccccc")
	if _, err := app.store.removeOwned("admin", job.ID, ""); err != nil {
		t.Fatal(err)
	}
	app.store.mu.Lock()
	older := time.Now().UTC().Add(-40 * 24 * time.Hour)
	app.store.jobs[job.ID].DeletedAt = &older
	if err := app.store.saveJobsLocked(); err != nil {
		app.store.mu.Unlock()
		t.Fatal(err)
	}
	app.store.mu.Unlock()
	app.runTrashSweep("manual")
	if len(app.store.trashed("admin")) != 0 {
		t.Fatal("expired archive not removed")
	}
	if _, err := os.Stat(app.store.archiveDir(job.OwnerID, job.Author, job.PostID)); !os.IsNotExist(err) {
		t.Fatalf("old archive dir still exists: %v", err)
	}
	history, err := app.store.readTrashAudit()
	if err != nil || len(history) < 2 {
		t.Fatalf("trash audit missing %d: %v", len(history), err)
	}
	last := history[len(history)-1]
	if last.Trigger != "manual_summary" || last.Removed != 1 || last.Failed != 0 {
		t.Fatalf("wrong sweep summary %+v", last)
	}
	p := app.accounts.policy()
	p.Trash = TrashPolicy{AutoPurge: true, RetentionDays: 14}
	if err := app.accounts.changePolicy(p); err != nil {
		t.Fatal(err)
	}
	if err := validateTrashPolicy(TrashPolicy{RetentionDays: 0}); err == nil {
		t.Fatal("zero retention accepted")
	}
	if err := validateTrashPolicy(TrashPolicy{RetentionDays: 3651}); err == nil {
		t.Fatal("unbounded retention accepted")
	}
	app.Close()
	other, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if got := other.accounts.policy().Trash; !got.AutoPurge || got.RetentionDays != 14 {
		t.Fatalf("trash policy not persisted: %+v", got)
	}
	reloaded, err := other.store.readTrashAudit()
	if err != nil || len(reloaded) != len(history) {
		t.Fatalf("trash journal not persisted %d: %v", len(reloaded), err)
	}
}

func TestTrashSweepFailsClosedWithoutJournal(t *testing.T) {
	root := t.TempDir()
	app, err := New(Config{AdminUser: "admin", AdminPassword: "strong-password-123", DataDir: root, SecretKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	admin := testLogin(t, app, "admin", "strong-password-123")
	job := trashTestJob(t, app.store, "dddddddddddddddddddddddd")
	if _, err := app.store.removeOwned("admin", job.ID, ""); err != nil {
		t.Fatal(err)
	}
	app.store.mu.Lock()
	older := time.Now().UTC().Add(-40 * 24 * time.Hour)
	app.store.jobs[job.ID].DeletedAt = &older
	if err := app.store.saveJobsLocked(); err != nil {
		app.store.mu.Unlock()
		t.Fatal(err)
	}
	app.store.mu.Unlock()
	if err := os.WriteFile(filepath.Join(root, "trash-cleanup-log.json"), []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	app.runTrashSweep("manual")
	if len(app.store.trashed("admin")) != 1 {
		t.Fatal("purged archive even though audit journal was corrupt")
	}
	policyRequest(t, app, admin, "DELETE", "/api/trash/"+job.ID, "", 503)
	if _, err := os.Stat(app.store.archiveDir(job.OwnerID, job.Author, job.PostID)); err != nil {
		t.Fatal("original content removed despite failed audit", err)
	}
}

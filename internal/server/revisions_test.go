package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRevisionSnapshotsShareMediaButRestorePrivateState(t *testing.T) {
	app, err := New(Config{
		DataDir: t.TempDir(), AdminUser: "admin",
		AdminPassword: "strong-password-123", SecretKey: testKey(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	owner := testLogin(t, app, "admin", "strong-password-123")
	other := testMember(t, app, "revisionmember")
	captured := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	id := "1234567890abcdef12345678"
	job := Job{
		ID: id, OwnerID: "admin", Author: "creator", PostID: "post-one",
		Title: "First", State: "done", SharedForAll: true,
		SharedWith: []string{"revisionmember"}, AutoSync: true,
		ArchiveUpdatedAt: captured,
		Subscription: &CapturedSubscription{Level: "Tier A", Paid: true, ObservedAt: captured, Source: "boosty_subscriptions"},
	}
	if err := app.store.upsert(job); err != nil {
		t.Fatal(err)
	}
	base := app.store.archiveDir(job.OwnerID, job.Author, job.PostID)
	if err := os.MkdirAll(filepath.Join(base, "media"), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("original media bytes")
	if err := os.WriteFile(filepath.Join(base, "media", "video.mp4"), original, 0600); err != nil {
		t.Fatal(err)
	}
	first := ArchiveManifest{
		Author: job.Author, PostID: job.PostID, Title: "First", CapturedAt: captured,
		Subscription: job.Subscription, Assets: []ArchiveAsset{
			{Filename: "video.mp4", Size: int64(len(original)), Type: "ok_video"},
		},
	}
	if err := writeJSON(filepath.Join(base, "manifest.json"), first); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(base, "index.html"), []byte("old HTML")); err != nil {
		t.Fatal(err)
	}
	rev, err := app.store.saveArchiveRevision(job)
	if err != nil || !validRevisionID(rev) {
		t.Fatalf("snapshot: %s %v", rev, err)
	}
	historic := filepath.Join(app.store.revisionsDir(job), rev)
	oldInfo, err := os.Stat(filepath.Join(historic, "media", "video.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	newInfo, err := os.Stat(filepath.Join(base, "media", "video.mp4"))
	if err != nil || !os.SameFile(oldInfo, newInfo) {
		t.Fatal("revision copied video rather than preserving a shared inode", err)
	}
	if err := writeAtomic(filepath.Join(base, "media", "video.mp4"), []byte("new media bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(base, "index.html"), []byte("new HTML")); err != nil {
		t.Fatal(err)
	}
	next := first
	next.Title = "Second"
	next.CapturedAt = captured.Add(7 * 24 * time.Hour)
	next.Assets = []ArchiveAsset{{Filename: "video.mp4", Size: int64(len("new media bytes")), Type: "ok_video"}}
	next.Subscription = &CapturedSubscription{Level: "Tier B", Paid: true, ObservedAt: next.CapturedAt, Source: "boosty_subscriptions"}
	if err := writeJSON(filepath.Join(base, "manifest.json"), next); err != nil {
		t.Fatal(err)
	}
	job.Title = "Second"
	job.Subscription = next.Subscription
	if err := app.store.upsert(job); err != nil {
		t.Fatal(err)
	}
	path := "/library/" + id + "/revisions/" + rev + "/index.html"
	testArchiveResponse(t, app, other, "GET", path, "", 404)
	testArchiveResponse(t, app, other, "GET", "/api/jobs/"+id+"/revisions", "", 404)
	view := testArchiveResponse(t, app, owner, "GET", path, "", 200)
	if !strings.Contains(string(view), "old HTML") {
		t.Fatal("historical content corrupted by current update")
	}
	restored, err := app.store.restoreArchiveRevision("admin", id, rev)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Title != "First" || restored.Subscription == nil || restored.Subscription.Level != "Tier A" {
		t.Fatalf("wrong revision restored: %+v", restored)
	}
	if restored.SharedForAll || len(restored.SharedWith) != 0 || restored.AutoSync {
		t.Fatal("history restore reopened shared access or auto-sync")
	}
	originalBytes, err := os.ReadFile(filepath.Join(base, "media", "video.mp4"))
	if err != nil || string(originalBytes) != string(original) {
		t.Fatalf("restored bytes wrong: %s %v", originalBytes, err)
	}
	revisions, err := app.store.listArchiveRevisions(restored)
	if err != nil || len(revisions) < 2 {
		t.Fatalf("restore did not preserve previous current version: %+v %v", revisions, err)
	}
	if _, err := app.store.restoreArchiveRevision("revisionmember", id, rev); err == nil {
		t.Fatal("cross-owner restoration succeeded")
	}
	if err := app.store.upsert(Job{
		ID: id, OwnerID: "admin", Author: job.Author, PostID: job.PostID, State: "running",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.restoreArchiveRevision("admin", id, rev); err == nil {
		t.Fatal("restored while archiver is running")
	}
	if err := app.store.upsert(restored); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.removeOwned("admin", id, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.purgeOwned("admin", id, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(app.store.revisionsDir(job)); !os.IsNotExist(err) {
		t.Fatalf("purge did not reclaim revision history: %v", err)
	}
}

func TestRevisionPruningKeepsRecentSnapshots(t *testing.T) {
	store, err := newStorage(t.TempDir(), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	job := Job{ID: "abcdefabcdefabcdefabcdef", OwnerID: "admin", Author: "creator", PostID: "post1", State: "done"}
	base := store.archiveDir(job.OwnerID, job.Author, job.PostID)
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(base, "index.html"), []byte("HTML")); err != nil {
		t.Fatal(err)
	}
	if err := store.pruneArchiveRevisions(job); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxArchiveRevisions+3; i++ {
		man := ArchiveManifest{
			Author: job.Author, PostID: job.PostID, Title: "Snapshot",
			CapturedAt: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour),
		}
		if err := writeJSON(filepath.Join(base, "manifest.json"), man); err != nil {
			t.Fatal(err)
		}
		if _, err := store.saveArchiveRevision(job); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.pruneArchiveRevisions(job); err != nil {
		t.Fatal(err)
	}
	history, err := store.listArchiveRevisions(job)
	if err != nil || len(history) != maxArchiveRevisions {
		t.Fatalf("want %d snapshots, got %d: %v", maxArchiveRevisions, len(history), err)
	}
	oldest := history[len(history)-1]
	if oldest.CapturedAt != time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC) {
		t.Fatalf("incorrect oldest retained snapshot: %+v", oldest)
	}
}

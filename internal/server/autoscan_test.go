package server

import (
	"context"
	"testing"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func TestAuthorAutoMonitoringReschedulesLatestImportOnly(t *testing.T) {
	root := t.TempDir()
	key := testKey(t)
	s, err := newStorage(root, key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s.mu.Lock()
	s.imports["older"] = &AuthorImport{ID: "older", OwnerID: "owner", Author: "creator", State: "done", AutoSync: true, MaxPosts: 0, CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now.Add(-48 * time.Hour)}
	s.imports["latest"] = &AuthorImport{ID: "latest", OwnerID: "owner", Author: "creator", State: "done", AutoSync: true, MaxPosts: 0, CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now.Add(-13 * time.Hour)}
	s.imports["other"] = &AuthorImport{ID: "other", OwnerID: "other", Author: "creator", State: "done", AutoSync: false, MaxPosts: 20, CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now.Add(-13 * time.Hour)}
	if err = s.saveImportsLocked(); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()
	ids, err := s.scheduleDueImports(now, 12*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "latest" {
		t.Fatalf("scheduled wrong imports: %v", ids)
	}
	current := s.importsFor("owner")
	for _, v := range current {
		if v.ID == "latest" && (!v.Incremental || v.MaxPosts != 50 || v.State != "queued" || v.Scanned != 0) {
			t.Fatalf("bad scheduled import: %+v", v)
		}
	}
	ids, err = s.scheduleDueImports(now.Add(time.Hour), 12*time.Hour, 10)
	if err != nil || len(ids) != 0 {
		t.Fatalf("duplicate scheduled work: %v / %v", ids, err)
	}
	reloaded, err := newStorage(root, key)
	if err != nil {
		t.Fatal(err)
	}
	if ids = reloaded.queuedImports(); len(ids) != 1 || ids[0] != "latest" {
		t.Fatalf("missing queued import after restart: %v", ids)
	}
}

func TestAuthorScheduleCanBeDisabledByOwner(t *testing.T) {
	s, err := newStorage(t.TempDir(), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	s.mu.Lock()
	s.imports["one"] = &AuthorImport{ID: "one", OwnerID: "owner", Author: "creator", State: "done", AutoSync: true, CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-24 * time.Hour)}
	s.jobs["post"] = &Job{ID: "post", OwnerID: "owner", Author: "creator", AutoSync: true}
	s.mu.Unlock()
	if err := s.setImportSchedule("intruder", "one", false); err == nil {
		t.Fatal("other user changed schedule")
	}
	if err := s.setImportSchedule("owner", "one", false); err != nil {
		t.Fatal(err)
	}
	if s.jobs["post"].AutoSync {
		t.Fatal("post update still enabled")
	}
	ids, err := s.scheduleDueImports(now, 12*time.Hour, 10)
	if err != nil || len(ids) != 0 {
		t.Fatalf("disabled author was scheduled: %v, %v", ids, err)
	}
	if err := s.setImportSchedule("owner", "one", true); err != nil {
		t.Fatal(err)
	}
	if !s.jobs["post"].AutoSync {
		t.Fatal("post update not reenabled")
	}
}

func TestIncrementalImportStopsAtExistingPost(t *testing.T) {
	app, err := New(Config{DataDir: t.TempDir(), AdminUser: "admin", AdminPassword: "strong-password-123", SecretKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	old := Job{ID: "existing", OwnerID: "admin", Author: "creator", PostID: "post100", Title: "Known", State: "done"}
	if err = app.store.upsert(old); err != nil {
		t.Fatal(err)
	}
	imp := AuthorImport{ID: "scan", OwnerID: "admin", Author: "creator", State: "running", Incremental: true, MaxPosts: 50, IncludeVideos: true}
	app.store.mu.Lock()
	app.store.imports[imp.ID] = &imp
	if err = app.store.saveImportsLocked(); err != nil {
		t.Fatal(err)
	}
	app.store.mu.Unlock()
	calls := 0
	app.feedSource = func(_ context.Context, _, _, _ string) (boosty.FeedResult, error) {
		calls++
		return boosty.FeedResult{Posts: []boosty.FeedPost{
			{ID: "post101", Title: "New", HasAccess: true},
			{ID: "post100", Title: "Known", HasAccess: true},
			{ID: "post099", Title: "Older", HasAccess: true},
		}}, nil
	}
	if err = app.runImport(&imp); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || imp.Scanned != 2 || imp.Queued != 1 || imp.Skipped != 1 {
		t.Fatalf("unexpected scan progress: %+v, calls=%d", imp, calls)
	}
	if jobs := app.store.all(); len(jobs) != 2 {
		t.Fatalf("expected only one new post, got %d", len(jobs))
	}
}

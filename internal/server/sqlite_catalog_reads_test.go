package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSQLiteCatalogReadsOwnerScopeAndJSONACL(t *testing.T) {
	t.Setenv("BD_SQLITE_CATALOG_READS", "1")
	app, _ := sqliteShadowApp(t)
	if !app.catalogSQLReads {
		t.Fatal("experimental catalog reads not enabled")
	}
	owner := testLogin(t, app, "admin", "strong-password-123")
	viewer := testMember(t, app, "catalogviewer")
	member, ok := app.accounts.userByName("catalogviewer")
	if !ok {
		t.Fatal("missing catalog member")
	}
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	shared := Job{
		ID: strings.Repeat("7", 24), OwnerID: "admin", Author: "writer",
		PostID: "public-post", Title: "Shared SQL indexed",
		State: "done", SharedWith: []string{member.ID}, CreatedAt: now,
	}
	private := Job{
		ID: strings.Repeat("8", 24), OwnerID: "admin", Author: "writer",
		PostID: "private-post", Title: "Private SQL indexed",
		State: "done", CreatedAt: now,
	}
	for _, job := range []Job{shared, private} {
		if err := app.store.upsert(job); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.store.syncSQLiteShadow(context.Background()); err != nil {
		t.Fatalf("failed to prepare SQL projection: %v", err)
	}
	mine, sqlUsed := app.store.catalogJobs(context.Background(), "admin", "mine", true)
	if !sqlUsed || len(mine) != 2 {
		t.Fatalf("owner catalog SQL index not used: used=%t jobs=%d", sqlUsed, len(mine))
	}
	items, sqlUsed := app.store.catalogJobs(context.Background(), member.ID, "shared", true)
	if !sqlUsed || len(items) != 1 || items[0].ID != shared.ID {
		t.Fatalf("wrong SQL shared inventory: %t %+v", sqlUsed, items)
	}
	request := func(cookie *http.Cookie, path string) catalogPage {
		t.Helper()
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, reqAs("GET", path, "", cookie))
		if rec.Code != http.StatusOK {
			t.Fatalf("catalog %s returned HTTP %d: %s", path, rec.Code, rec.Body.String())
		}
		var p catalogPage
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := request(viewer, "/api/catalog?scope=shared")
	if p.Total != 1 || len(p.Items) != 1 || p.Items[0].Title != shared.Title ||
		p.Items[0].OwnerID != "" || p.Items[0].Subscription != nil {
		t.Fatalf("SQL-backed HTTP catalog exposed wrong data: %+v", p)
	}
	p = request(owner, "/api/catalog?scope=mine&sort=title&order=asc")
	if p.Total != 2 || p.Items[0].Title != private.Title {
		t.Fatalf("owner catalog SQL filtering/sorting changed: %+v", p)
	}
	// A forged SQL-only grant must not introduce an unauthorized candidate.
	if _, err := app.store.shadow.meta.db.Exec(
		"INSERT INTO archive_grants(archive_id,grantee_owner_id,created_at) VALUES(?,?,?)",
		private.ID, member.ID, sqliteDate(now)); err != nil {
		t.Fatal(err)
	}
	items, sqlUsed = app.store.catalogJobs(context.Background(), member.ID, "shared", true)
	if sqlUsed || len(items) != 4 {
		t.Fatalf("forged SQL grant should force full JSON fallback: %t, %d", sqlUsed, len(items))
	}
	p = request(viewer, "/api/catalog?scope=shared")
	if p.Total != 1 || p.Items[0].ID != shared.ID {
		t.Fatalf("forged SQL grant bypassed JSON ACL: %+v", p)
	}
	if _, err := app.store.shadow.meta.db.Exec(
		"DELETE FROM archive_grants WHERE archive_id=? AND grantee_owner_id=?",
		private.ID, member.ID); err != nil {
		t.Fatal(err)
	}

	// A pending JSON change must not be hidden by a stale SQL query. Holding
	// the synchronizer lock makes this deterministic even with its worker on.
	app.store.shadow.syncMu.Lock()
	private.Title = "New JSON title"
	if err := app.store.upsert(private); err != nil {
		app.store.shadow.syncMu.Unlock()
		t.Fatal(err)
	}
	_, sqlUsed = app.store.catalogJobs(context.Background(), "admin", "mine", true)
	if sqlUsed {
		app.store.shadow.syncMu.Unlock()
		t.Fatal("catalog used a stale SQL generation after JSON update")
	}
	app.store.shadow.syncMu.Unlock()
	if err := app.store.syncSQLiteShadow(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, sqlUsed = app.store.catalogJobs(context.Background(), "admin", "mine", true)
	if !sqlUsed {
		t.Fatal("SQL catalog did not recover after reconciliation")
	}
	p = request(owner, "/api/catalog?scope=mine&q=New%20JSON%20title")
	if p.Total != 1 || p.Items[0].ID != private.ID {
		t.Fatalf("SQL catalog failed to show current JSON title: %+v", p)
	}
}

func TestSQLiteCatalogReadsRequiresShadowMode(t *testing.T) {
	t.Setenv("BD_SQLITE_CATALOG_READS", "1")
	t.Setenv("BD_SQLITE_SHADOW", "")
	app, err := New(Config{
		AdminUser: "admin", AdminPassword: "strong-password-123",
		SecretKey: testKey(t), DataDir: t.TempDir(),
	})
	if app != nil {
		app.Close()
		t.Fatal("SQL catalog activated without a synchronized shadow")
	}
	if err == nil || !strings.Contains(err.Error(), "BD_SQLITE_SHADOW") {
		t.Fatalf("missing safety gate for SQL catalog: %v", err)
	}
}

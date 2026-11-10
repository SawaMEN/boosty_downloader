package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCatalogProtectsSharedPostsAndPages(t *testing.T) {
	app, err := New(Config{AdminUser: "admin", AdminPassword: "strong-password-123", SecretKey: testKey(t), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	owner := testLogin(t, app, "admin", "strong-password-123")
	alice := testMember(t, app, "memberalice")
	bob := testMember(t, app, "memberbob")
	base := time.Date(2026, 11, 10, 10, 0, 0, 0, time.UTC)
	user, _ := app.accounts.userByName("memberalice")
	jobs := []Job{
		{ID: "000000000000000000000001", OwnerID: "admin", Title: "First", Author: "artist", State: "done", CreatedAt: base, ArchiveUpdatedAt: base, SharedForAll: true, Error: "private worker error"},
		{ID: "000000000000000000000002", OwnerID: "admin", Title: "Second", Author: "artist", State: "partial", CreatedAt: base.Add(time.Hour), ArchiveUpdatedAt: base.Add(time.Hour), SharedWith: []string{user.ID}},
		{ID: "000000000000000000000003", OwnerID: "admin", Title: "Secret", Author: "private", State: "done", CreatedAt: base.Add(2*time.Hour)},
	}
	for _, job := range jobs {
		if err := app.store.upsert(job); err != nil {
			t.Fatal(err)
		}
	}
	request := func(cookie *http.Cookie, url string, expected int) catalogPage {
		t.Helper()
		w := httptest.NewRecorder()
		app.Handler().ServeHTTP(w, reqAs("GET", url, "", cookie))
		if w.Code != expected {
			t.Fatalf("%s status %d expected %d: %s", url, w.Code, expected, w.Body.String())
		}
		if expected != 200 {
			return catalogPage{}
		}
		var p catalogPage
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	p := request(alice, "/api/catalog?scope=shared&perPage=1", 200)
	if p.Total != 2 || p.Pages != 2 || len(p.Items) != 1 || p.Items[0].Title != "Second" {
		t.Fatalf("unexpected first page: %+v", p)
	}
	if p.Items[0].OwnerID != "" || len(p.Items[0].SharedWith) != 0 {
		t.Fatal("private fields exposed")
	}
	p = request(alice, "/api/catalog?scope=shared&page=2&perPage=1", 200)
	if len(p.Items) != 1 || p.Items[0].Title != "First" || p.Items[0].Error != "" {
		t.Fatalf("unexpected second page or private error: %+v", p)
	}
	p = request(bob, "/api/catalog?scope=shared", 200)
	if p.Total != 1 || p.Items[0].Title != "First" {
		t.Fatalf("shared post leaked: %+v", p)
	}
	if p = request(alice, "/api/catalog?scope=shared&q=Secret", 200); p.Total != 0 {
		t.Fatal("private search result exposed")
	}
	if p = request(owner, "/api/catalog?scope=mine&author=ART&sort=title&order=asc", 200); p.Total != 2 || p.Items[0].Title != "First" {
		t.Fatalf("incorrect filtering/sorting: %+v", p)
	}
	for _, bad := range []string{"scope=invalid", "page=0", "perPage=101", "sort=bad", "order=bad", "status=bogus"} {
		request(alice, "/api/catalog?" + bad, 400)
	}
	request(nil, "/api/catalog?scope=shared", 401)
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func importRequest(t *testing.T, app *App, method, path, body string, cookie *http.Cookie, want int) []byte {
	t.Helper()
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, reqAs(method, path, body, cookie))
	if w.Code != want {
		t.Fatalf("%s %s status=%d want=%d: %s", method, path, w.Code, want, w.Body.String())
	}
	return w.Body.Bytes()
}
func TestSubscriptionSelectionAndPersistentDiscovery(t *testing.T) {
	dir := t.TempDir()
	app, err := New(Config{DataDir: dir, AdminUser: "admin", AdminPassword: "strong-password-123", SecretKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	owner := testLogin(t, app, "admin", "strong-password-123")
	app.subscriptionSource = func(_ context.Context, owner string) (boosty.SubscriptionList, error) {
		return boosty.SubscriptionList{Authors: []boosty.SubscriptionAuthor{{Blog: "creator", Title: "Автор", Paid: true}, {Blog: "second", Title: "Другой"}}}, nil
	}
	app.feedSource = func(_ context.Context, _, author, offset string) (boosty.FeedResult, error) {
		if author != "creator" {
			return boosty.FeedResult{}, fmt.Errorf("wrong author: %s", author)
		}
		if offset != "" {
			t.Errorf("unexpected offset: %s", offset)
		}
		return boosty.FeedResult{Posts: []boosty.FeedPost{{ID: "post1", Title: "A", HasAccess: true}, {ID: "post2", Title: "B", HasAccess: false}, {ID: "post3", Title: "C", HasAccess: true}}}, nil
	}
	importRequest(t, app, "POST", "/api/subscriptions/import", `{"authors":["not-subscribed"],"maxPosts":20}`, owner, 403)
	importRequest(t, app, "POST", "/api/subscriptions/import", `{"authors":["creator","creator"],"maxPosts":20}`, owner, 400)
	response := importRequest(t, app, "POST", "/api/subscriptions/import", `{"authors":["creator"],"maxPosts":20,"includeComments":true,"includeFiles":true,"includeVideos":true}`, owner, 202)
	var created struct {
		Imports []AuthorImport `json:"imports"`
	}
	if err = json.Unmarshal(response, &created); err != nil || len(created.Imports) != 1 {
		t.Fatalf("bad create: %v %s", err, response)
	}
	id := created.Imports[0].ID
	deadline := time.Now().Add(4 * time.Second)
	for {
		records := app.store.importsFor("admin")
		if len(records) == 1 && records[0].State == "done" {
			if records[0].Scanned != 3 || records[0].Queued != 2 || records[0].Skipped != 1 {
				t.Fatalf("bad counters: %+v", records[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("import never completed: %+v", records)
		}
		time.Sleep(20 * time.Millisecond)
	}
	jobs := app.store.all()
	if len(jobs) != 2 {
		t.Fatalf("want 2 jobs got %d", len(jobs))
	}
	for _, j := range jobs {
		if !j.IncludeComments || !j.IncludeFiles || !j.IncludeVideos || j.OwnerID != "admin" {
			t.Fatalf("invalid job: %+v", j)
		}
	}
	// Reimport does not produce duplicate post archives; imports are independent histories.
	importRequest(t, app, "POST", "/api/subscriptions/import", `{"authors":["creator"],"maxPosts":20}`, owner, 202)
	time.Sleep(120 * time.Millisecond)
	if len(app.store.all()) != 2 {
		t.Fatal("created duplicate jobs")
	}
	imported, err := newStorage(dir, app.cfg.SecretKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(imported.importsFor("admin")) < 1 {
		t.Fatal("import history not persisted")
	}
	if id == "" {
		t.Fatal("missing import ID")
	}
}
func TestSubscriptionImportIsAccountScoped(t *testing.T) {
	app, err := New(Config{DataDir: t.TempDir(), AdminUser: "admin", AdminPassword: "strong-password-123", SecretKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	owner := testLogin(t, app, "admin", "strong-password-123")
	if err = app.accounts.setRegistration(true); err != nil {
		t.Fatal(err)
	}
	request := reqAs("POST", "/api/account/register", `{"username":"secondmember","password":"another-secure-pass"}`, nil)
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, request)
	if w.Code != 201 {
		t.Fatalf("register %d %s", w.Code, w.Body.String())
	}
	other := testLogin(t, app, "secondmember", "another-secure-pass")
	app.subscriptionSource = func(_ context.Context, _ string) (boosty.SubscriptionList, error) {
		return boosty.SubscriptionList{Authors: []boosty.SubscriptionAuthor{{Blog: "author1"}}}, nil
	}
	app.feedSource = func(_ context.Context, _, _, _ string) (boosty.FeedResult, error) {
		time.Sleep(50 * time.Millisecond)
		return boosty.FeedResult{}, nil
	}
	reply := importRequest(t, app, "POST", "/api/subscriptions/import", `{"authors":["author1"],"maxPosts":20}`, owner, 202)
	var v struct {
		Imports []AuthorImport `json:"imports"`
	}
	_ = json.Unmarshal(reply, &v)
	if len(v.Imports) != 1 {
		t.Fatal("missing import")
	}
	id := v.Imports[0].ID
	otherImports := importRequest(t, app, "GET", "/api/imports", "", other, 200)
	if strings.TrimSpace(string(otherImports)) != "[]" {
		t.Fatalf("leaked imports: %s", otherImports)
	}
	importRequest(t, app, "POST", "/api/imports/"+id+"/cancel", "", other, 409)
	importRequest(t, app, "POST", "/api/imports/"+id+"/cancel", "", owner, 200)
	time.Sleep(70 * time.Millisecond)
	if imports := app.store.importsFor("admin"); len(imports) != 1 || imports[0].State != "canceled" {
		t.Fatalf("cancel overwritten: %+v", imports)
	}
}

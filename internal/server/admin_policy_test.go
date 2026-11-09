package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func policyRequest(t *testing.T, app *App, cookie *http.Cookie, method, path, body string, status int) []byte {
	t.Helper()
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, reqAs(method, path, body, cookie))
	if recorder.Code != status {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, recorder.Code, status, recorder.Body.String())
	}
	return recorder.Body.Bytes()
}

func TestAdminPolicyPersistenceAndIndividualPermissionEnforcement(t *testing.T) {
	root := t.TempDir()
	cfg := Config{AdminUser: "admin", AdminPassword: "strong-password-123", DataDir: root, SecretKey: testKey(t), SyncInterval: 24 * time.Hour}
	app, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	admin := testLogin(t, app, "admin", "strong-password-123")
	member := testMember(t, app, "policyuser")
	user, found := app.accounts.userByName("policyuser")
	if !found {
		t.Fatal("member missing")
	}
	initial := app.accounts.policy()
	if initial.SyncIntervalHours != 24 || initial.MaxScanPosts != 50 || initial.CompressionLevel != 3 || !initial.DeduplicateMedia || initial.Defaults["autoArchive"] {
		t.Fatalf("unexpected defaults: %+v", initial)
	}
	policyRequest(t, app, member, "GET", "/api/admin/policy", "", 403)
	policyRequest(t, app, member, "GET", "/api/admin/users", "", 403)
	policyRequest(t, app, member, "PUT", "/api/admin/users/"+user.ID+"/permissions", `{"overrides":{}}`, 403)
	policyRequest(t, app, member, "GET", "/api/post?url=invalid", "", 400) // valid capability reaches URL validator
	// A global default policy can deny features without affecting existing logins.
	initial.Defaults["browsePosts"] = false
	initial.Defaults["connectBoosty"] = false
	initial.Defaults["createArchives"] = false
	initial.Defaults["browseShared"] = false
	initial.Defaults["saveVideos"] = false
	initial.RegistrationOpen = false
	initial.SyncIntervalHours = 24
	initial.MaxScanPosts = 30
	initial.MaxImportPosts = 120
	initial.CompressionLevel = 9
	initial.DeduplicateMedia = false
	payload, _ := json.Marshal(initial)
	policyRequest(t, app, admin, "PUT", "/api/admin/policy", string(payload), 200)
	policyRequest(t, app, member, "GET", "/api/feed?author=creator", "", 403)
	policyRequest(t, app, member, "GET", "/api/shared", "", 403)
	policyRequest(t, app, member, "POST", "/api/auth", `{"method":"tokens","accessToken":"test"}`, 403)
	policyRequest(t, app, member, "POST", "/api/jobs", `{"url":"https://boosty.to/creator/posts/p1"}`, 403)
	// The administrator may grant select privileges to exactly one account.
	policyRequest(t, app, admin, "PUT", "/api/admin/users/"+user.ID+"/permissions", `{"overrides":{"browsePosts":true,"createArchives":true}}`, 200)
	account := policyRequest(t, app, member, "GET", "/api/account", "", 200)
	if !strings.Contains(string(account), `"browsePosts":true`) || !strings.Contains(string(account), `"connectBoosty":false`) {
		t.Fatalf("unexpected effective permissions: %s", account)
	}
	policyRequest(t, app, member, "GET", "/api/post?url=invalid", "", 400)
	policyRequest(t, app, member, "POST", "/api/jobs", `{"url":"http://127.0.0.1/posts/1"}`, 400)
	policyRequest(t, app, member, "POST", "/api/jobs", `{"url":"https://boosty.to/creator/posts/p1","includeVideos":true}`, 403)
	// Unknown capabilities and invalid scheduler settings must never be persisted.
	bad := initial
	bad.Defaults = map[string]bool{"unknown": true}
	raw, _ := json.Marshal(bad)
	policyRequest(t, app, admin, "PUT", "/api/admin/policy", string(raw), 400)
	bad = initial
	bad.SyncIntervalHours = 999
	raw, _ = json.Marshal(bad)
	policyRequest(t, app, admin, "PUT", "/api/admin/policy", string(raw), 400)
	bad = initial
	bad.CompressionLevel = 20
	raw, _ = json.Marshal(bad)
	policyRequest(t, app, admin, "PUT", "/api/admin/policy", string(raw), 400)
	// No password hashes are ever exposed through the admin listing.
	listing := policyRequest(t, app, admin, "GET", "/api/admin/users", "", 200)
	if strings.Contains(string(listing), "passwordHash") || strings.Contains(string(listing), "pbkdf2") {
		t.Fatal("password hashes leaked through user list")
	}
	app.Close()
	other, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.accounts.policy().SyncIntervalHours != 24 || other.accounts.policy().MaxScanPosts != 30 || other.accounts.policy().MaxImportPosts != 120 || other.accounts.policy().CompressionLevel != 9 || other.accounts.policy().DeduplicateMedia {
		t.Fatal("policy lost on restart")
	}
	if !other.accounts.allowed(user.ID, "browsePosts") || other.accounts.allowed(user.ID, "connectBoosty") || other.accounts.allowed(user.ID, "saveVideos") {
		t.Fatal("user overrides lost on restart")
	}
	if other.accounts.registration() {
		t.Fatal("closed registration reopened on restart")
	}
	b, err := os.ReadFile(filepath.Join(root, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "strong-password-123") {
		t.Fatal("plaintext password found on disk")
	}
}

func TestAdminPolicyAuthorSchedulerOptInRevocationAndRecovery(t *testing.T) {
	app, err := New(Config{AdminUser: "admin", AdminPassword: "strong-password-123", DataDir: t.TempDir(), SecretKey: testKey(t), SyncInterval: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	_ = testMember(t, app, "scheduledmember")
	member, _ := app.accounts.userByName("scheduledmember")
	now := time.Now().UTC()
	app.store.mu.Lock()
	app.store.imports["due"] = &AuthorImport{ID: "due", OwnerID: member.ID, Author: "creator", State: "done", AutoSync: true, MaxPosts: 120, CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-25 * time.Hour)}
	if err = app.store.saveImportsLocked(); err != nil {
		t.Fatal(err)
	}
	app.store.mu.Unlock()
	filter := func(id string) bool {
		return app.accounts.allowed(id, "autoArchive") && app.accounts.allowed(id, "importAuthors")
	}
	ids, err := app.store.scheduleDueImportsAllowed(now, app.syncInterval(), 10, filter)
	if err != nil || len(ids) != 0 {
		t.Fatalf("disabled capability was scheduled: %v / %v", ids, err)
	}
	if err = app.accounts.setOverrides(member.ID, map[string]bool{"autoArchive": true}); err != nil {
		t.Fatal(err)
	}
	ids, err = app.store.scheduleDueImportsAllowed(now, app.syncInterval(), 10, filter)
	if err != nil || len(ids) != 1 || ids[0] != "due" {
		t.Fatalf("not scheduled after permission: %v / %v", ids, err)
	}
	v := app.store.importsFor(member.ID)[0]
	if v.MaxPosts != 120 || !v.Incremental || v.State != "queued" {
		t.Fatalf("initial scan settings destroyed: %+v", v)
	}
	// Revoking permission cannot let the queued import read more content.
	if err = app.accounts.setOverrides(member.ID, map[string]bool{"autoArchive": false}); err != nil {
		t.Fatal(err)
	}
	if err = app.runImport(&v); err == nil {
		t.Fatal("revoked permission still permitted import")
	}
	// Admin can stop the schedule server-wide without losing saved preferences.
	policy := app.accounts.policy()
	policy.SyncIntervalHours = 0
	if err = app.accounts.changePolicy(policy); err != nil {
		t.Fatal(err)
	}
	if app.syncInterval() != 0 {
		t.Fatal("scheduler not disabled")
	}
}

func TestAdminCannotGrantUnknownPermissions(t *testing.T) {
	acc, err := newAccounts(t.TempDir(), "admin", "strong-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if err = acc.initializePolicy(24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	member, err := acc.register("testperson", "very-long-password-here")
	if err == nil {
		t.Fatal("registration should be closed")
	}
	if err = acc.setRegistration(true); err != nil {
		t.Fatal(err)
	}
	member, err = acc.register("testperson", "very-long-password-here")
	if err != nil {
		t.Fatal(err)
	}
	if err = acc.setOverrides(member.ID, map[string]bool{"not-a-feature": true}); err == nil {
		t.Fatal("unknown permission accepted")
	}
	if err = acc.setOverrides("admin", map[string]bool{"autoArchive": false}); err == nil {
		t.Fatal("admin rights overridden")
	}
}

func TestAdminCanBlockPublishingToAllAndMemberCanStillRevoke(t *testing.T) {
	app, err := New(Config{AdminUser: "admin", AdminPassword: "strong-password-123", DataDir: t.TempDir(), SecretKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	admin := testLogin(t, app, "admin", "strong-password-123")
	member := testMember(t, app, "sharepolicy")
	owner, _ := app.accounts.userByName("sharepolicy")
	id := "1af051d760ef831bc993abc2"
	if err = app.store.upsert(Job{ID: id, OwnerID: owner.ID, Author: "creator", PostID: "post", State: "done"}); err != nil {
		t.Fatal(err)
	}
	path := "/api/jobs/" + id + "/visibility"
	policyRequest(t, app, member, "PUT", path, `{"everyone":true}`, 403)
	policyRequest(t, app, admin, "PUT", "/api/admin/users/"+owner.ID+"/permissions", `{"overrides":{"sharePublic":true}}`, 200)
	policyRequest(t, app, member, "PUT", path, `{"everyone":true}`, 200)
	policyRequest(t, app, admin, "PUT", "/api/admin/users/"+owner.ID+"/permissions", `{"overrides":{"sharePublic":false}}`, 200)
	policyRequest(t, app, member, "PUT", path, `{"everyone":false}`, 200)
	archived, _ := app.store.get(id)
	if archived.SharedForAll {
		t.Fatal("revocation ignored")
	}
}

func TestAdminCanControlUserAuthorAutoScanToggle(t *testing.T) {
	app, err := New(Config{AdminUser: "admin", AdminPassword: "strong-password-123", DataDir: t.TempDir(), SecretKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	admin := testLogin(t, app, "admin", "strong-password-123")
	member := testMember(t, app, "scannedmember")
	owner, _ := app.accounts.userByName("scannedmember")
	app.store.mu.Lock()
	app.store.imports["scan-policy"] = &AuthorImport{ID: "scan-policy", OwnerID: owner.ID, Author: "creator", State: "done", AutoSync: false}
	if err = app.store.saveImportsLocked(); err != nil {
		t.Fatal(err)
	}
	app.store.mu.Unlock()
	path := "/api/imports/scan-policy/schedule"
	policyRequest(t, app, member, "PUT", path, `{"autoSync":true}`, 403)
	policyRequest(t, app, admin, "PUT", "/api/admin/users/"+owner.ID+"/permissions", `{"overrides":{"autoArchive":true}}`, 200)
	policyRequest(t, app, member, "PUT", path, `{"autoSync":true}`, 200)
	policyRequest(t, app, admin, "PUT", "/api/admin/users/"+owner.ID+"/permissions", `{"overrides":{"autoArchive":false}}`, 200)
	policyRequest(t, app, member, "PUT", path, `{"autoSync":true}`, 403)
	policyRequest(t, app, member, "PUT", path, `{"autoSync":false}`, 200)
}

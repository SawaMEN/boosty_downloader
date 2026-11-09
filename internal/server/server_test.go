package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/SawaMEN/boosty_downloader/internal/boosty"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		t.Fatal(e)
	}
	return base64.StdEncoding.EncodeToString(b)
}
func TestVaultEncrypted(t *testing.T) {
	dir := t.TempDir()
	key := testKey(t)
	s, e := newStorage(dir, key)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.saveTokens(boosty.Tokens{AccessToken: "super-secret-value"}); e != nil {
		t.Fatal(e)
	}
	b, e := osRead(filepath.Join(dir, "boosty-auth.enc"))
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(b, []byte("super-secret-value")) {
		t.Fatal("token persisted in plaintext")
	}
	tkn, e := s.loadTokens()
	if e != nil || tkn.AccessToken != "super-secret-value" {
		t.Fatal("vault roundtrip failed", e)
	}
	other, e := newStorage(dir, testKey(t))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = other.loadTokens(); e == nil {
		t.Fatal("decryption should fail with incorrect key")
	}
}
func osRead(path string) ([]byte, error) { return os.ReadFile(path) }
func testLogin(t *testing.T, app *App, user, pass string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/account/login", strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, user, pass)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-BD-Request", "1")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("login %d: %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			if !c.HttpOnly || !c.Secure {
				t.Fatal("insecure cookie")
			}
			return c
		}
	}
	t.Fatal("missing session cookie")
	return nil
}
func reqAs(method, path string, body string, cookie *http.Cookie) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("X-BD-Request", "1")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return r
}
func TestAuthentication(t *testing.T) {
	app, e := New(Config{AdminUser: "admin", AdminPassword: "strong-password-123", SecretKey: testKey(t), DataDir: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(app.Close)
	h := app.Handler()
	r := httptest.NewRequest("GET", "/api/jobs", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("want 401 got %d", w.Code)
	}
	cookie := testLogin(t, app, "admin", "strong-password-123")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, reqAs("GET", "/api/jobs", "", cookie))
	if w.Code != 200 {
		t.Fatalf("want 200 got %d body %s", w.Code, w.Body.String())
	}
	r = reqAs("POST", "/api/account/logout", "", cookie)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("logout %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, reqAs("GET", "/api/jobs", "", cookie))
	if w.Code != 401 {
		t.Fatal("logout did not revoke session")
	}
}
func TestRejectInvalidJobURL(t *testing.T) {
	app, e := New(Config{AdminUser: "admin", AdminPassword: "strong-password-123", SecretKey: testKey(t), DataDir: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(app.Close)
	cookie := testLogin(t, app, "admin", "strong-password-123")
	req := reqAs("POST", "/api/jobs", `{"url":"http://localhost/posts/1"}`, cookie)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("code %d", rec.Code)
	}
}
func TestMultiuserIsolation(t *testing.T) {
	app, e := New(Config{AdminUser: "admin", AdminPassword: "strong-password-123", SecretKey: testKey(t), DataDir: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(app.Close)
	admin := testLogin(t, app, "admin", "strong-password-123")
	req := reqAs("POST", "/api/admin/registration", `{"registrationOpen":true}`, admin)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("allow register %d", rec.Code)
	}
	req = reqAs("POST", "/api/account/register", `{"username":"member123","password":"some-very-long-passphrase"}`, nil)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatalf("register %d: %s", rec.Code, rec.Body.String())
	}
	member := rec.Result().Cookies()[0]
	req = reqAs("POST", "/api/auth", `{"accessToken":"special-separate-token","method":"tokens"}`, member)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("save user tokens %d %s", rec.Code, rec.Body.String())
	}
	req = reqAs("GET", "/api/auth", "", admin)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "true") {
		t.Fatal("member Boosty auth leaked to admin")
	}
	req = reqAs("POST", "/api/jobs", `{"url":"https://boosty.to/author/posts/00112233"}`, member)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != 202 {
		t.Fatalf("job %d: %s", rec.Code, rec.Body.String())
	}
	var job Job
	if e = json.Unmarshal(rec.Body.Bytes(), &job); e != nil {
		t.Fatal(e)
	}
	req = reqAs("GET", "/api/jobs", "", admin)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), job.ID) {
		t.Fatal("admin can see member jobs")
	}
	for _, path := range []string{"/api/jobs/" + job.ID + "/manifest", "/api/jobs/" + job.ID + "/zip", "/library/" + job.ID + "/index.html"} {
		req = reqAs("GET", path, "", admin)
		rec = httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, req)
		if rec.Code != 404 {
			t.Fatalf("cross-account leak on %s: %d", path, rec.Code)
		}
	}
}
func TestCSRFGuard(t *testing.T) {
	app, e := New(Config{AdminUser: "admin", AdminPassword: "strong-password-123", SecretKey: testKey(t), DataDir: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(app.Close)
	req := httptest.NewRequest("POST", "/api/account/login", strings.NewReader(`{"username":"admin","password":"strong-password-123"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("CSRF bypass %d", rec.Code)
	}
}
func TestArchiveHTMLSanitizesText(t *testing.T) {
	var out bytes.Buffer
	err := pageTemplate.Execute(&out, renderData{ArchiveManifest: ArchiveManifest{Title: "<script>alert(1)</script>"}, Body: []string{"<img src=x onerror=alert(1)>"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "<script>alert(1)</script>") || strings.Contains(out.String(), "<img src=x") {
		t.Fatal("HTML injection")
	}
}

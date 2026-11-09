package server

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
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
func TestAuthentication(t *testing.T) {
	app, e := New(Config{AdminUser: "admin", AdminPassword: "strong-password-123", SecretKey: testKey(t), DataDir: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	h := app.Handler()
	r := httptest.NewRequest(http.MethodGet, "/api/jobs", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("want 401 got %d", w.Code)
	}
	r = httptest.NewRequest("GET", "/api/jobs", nil)
	r.SetBasicAuth("admin", "strong-password-123")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("want 200 got %d body: %s", w.Code, w.Body.String())
	}
}
func TestRejectInvalidJobURL(t *testing.T) {
	app, e := New(Config{AdminUser: "admin", AdminPassword: "strong-password-123", SecretKey: testKey(t), DataDir: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	req := httptest.NewRequest("POST", "/api/jobs", strings.NewReader(`{"url":"http://localhost/posts/1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetBasicAuth("admin", "strong-password-123")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("code %d", rec.Code)
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

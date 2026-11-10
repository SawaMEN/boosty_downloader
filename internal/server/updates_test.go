package server

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdatePolicyValidation(t *testing.T) {
	p := defaultAutoUpdatePolicy()
	if p.AutoApply || p.Channel != "stable" || p.CheckHours != 24 {
		t.Fatalf("unsafe update defaults: %+v", p)
	}
	if err := validateAutoUpdatePolicy(p); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []AutoUpdatePolicy{{Channel: "http://invalid", CheckHours: 24}, {Channel: "stable", CheckHours: 0}, {Channel: "dev", CheckHours: 169}} {
		if err := validateAutoUpdatePolicy(bad); err == nil {
			t.Fatalf("accepted invalid policy %+v", bad)
		}
	}
}
func TestReadUpdateStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-status.json")
	v, err := readUpdateStatus(path)
	if err != nil || v.State != "not_checked" {
		t.Fatalf("uninitialized: %+v %v", v, err)
	}
	if err := os.WriteFile(path, []byte(`{"state":"available","latest":"v1.2.3","detail":"Доступно обновление"}`), 0600); err != nil {
		t.Fatal(err)
	}
	v, err = readUpdateStatus(path)
	if err != nil || v.State != "available" || v.Latest != "v1.2.3" {
		t.Fatalf("parsed: %+v %v", v, err)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readUpdateStatus(path); err == nil {
		t.Fatal("corrupt JSON accepted")
	}
	if err := os.WriteFile(path, []byte(`{"detail":"`+strings.Repeat("x", 1025)+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readUpdateStatus(path); err == nil {
		t.Fatal("unbounded text accepted")
	}
}
func TestReadUpdateStatusRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "target"), []byte(`{"state":"updated"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "target"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := readUpdateStatus(filepath.Join(dir, "link")); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestAutoUpdatePolicyPersistsAfterRestart(t *testing.T) {
	root := t.TempDir()
	acc, err := newAccounts(root, "admin", "strong-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := acc.initializePolicy(24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	p := acc.policy()
	if p.Updates.AutoApply || p.Updates.Channel != "stable" || p.Updates.CheckHours != 24 {
		t.Fatalf("unsafe default policy: %+v", p.Updates)
	}
	p.Updates = AutoUpdatePolicy{Channel: "dev", CheckHours: 6, AutoApply: true}
	if err := acc.changePolicy(p); err != nil {
		t.Fatal(err)
	}
	again, err := newAccounts(root, "admin", "strong-password-123")
	if err != nil {
		t.Fatal(err)
	}
	if err := again.initializePolicy(24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	actual := again.policy().Updates
	if !actual.AutoApply || actual.Channel != "dev" || actual.CheckHours != 6 {
		t.Fatalf("policy lost on restart: %+v", actual)
	}
}

func TestUpdateStatusRequiresAdmin(t *testing.T) {
	app, err := New(Config{AdminUser: "admin", AdminPassword: "strong-password-123", SecretKey: testKey(t), DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	handler := app.Handler()
	unauth := httptest.NewRecorder()
	handler.ServeHTTP(unauth, reqAs("GET", "/api/admin/updates", "", nil))
	if unauth.Code != 401 {
		t.Fatalf("unauthorized status=%d", unauth.Code)
	}
	member := testMember(t, app, "updateviewer")
	forbidden := httptest.NewRecorder()
	handler.ServeHTTP(forbidden, reqAs("GET", "/api/admin/updates", "", member))
	if forbidden.Code != 403 {
		t.Fatalf("member status=%d", forbidden.Code)
	}
	admin := testLogin(t, app, "admin", "strong-password-123")
	allowed := httptest.NewRecorder()
	handler.ServeHTTP(allowed, reqAs("GET", "/api/admin/updates", "", admin))
	if allowed.Code != 200 || !strings.Contains(allowed.Body.String(), "not_checked") {
		t.Fatalf("admin status=%d body=%s", allowed.Code, allowed.Body.String())
	}
}

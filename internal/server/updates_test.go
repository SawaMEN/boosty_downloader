package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

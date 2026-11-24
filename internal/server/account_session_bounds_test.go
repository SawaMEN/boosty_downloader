package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func TestBoundedLoginSessions(t *testing.T) {
	now := time.Now()
	accounts := map[string]Account{"a": {ID: "a"}, "b": {ID: "b"}}
	before := map[string]LoginSession{
		"expired": {UserID: "a", Expires: now},
		"orphan":  {UserID: "missing", Expires: now.Add(time.Hour)},
		"other":   {UserID: "b", Expires: now.Add(time.Hour)},
	}
	for i := 0; i < maxSessionsPerOwner+2; i++ {
		before[fmt.Sprint(i)] = LoginSession{UserID: "a", Expires: now.Add(time.Duration(i+1) * time.Minute)}
	}
	next := boundedLoginSessions(before, accounts, "a", now)
	if len(next) != maxSessionsPerOwner || next["other"].UserID != "b" ||
		next["0"].UserID != "" || next["expired"].UserID != "" || next["orphan"].UserID != "" ||
		len(before) != maxSessionsPerOwner+5 {
		t.Fatalf("session pruning changed another owner or input: %d", len(next))
	}
}

func TestSessionSaveFailureKeepsPreviousGeneration(t *testing.T) {
	root := t.TempDir()
	a := &Accounts{path: filepath.Join(root, "accounts.json"), data: accountFile{
		Accounts: map[string]Account{"a": {ID: "a"}},
		Sessions: map[string]LoginSession{"old": {UserID: "a", Expires: time.Now().Add(time.Hour)}},
	}}
	if err := os.Mkdir(a.path, 0700); err != nil {
		t.Fatal(err)
	}
	if token, err := a.session("a"); err == nil || token != "" {
		t.Fatal("failed session save succeeded")
	}
	if len(a.data.Sessions) != 1 || a.data.Sessions["old"].UserID != "a" {
		t.Fatal("failed session changed in-memory state")
	}
	if err := a.setRegistration(true); err == nil || a.registration() {
		t.Fatal("failed registration setting remained active")
	}
}

func TestLoginPeerStateBounded(t *testing.T) {
	now := time.Now()
	a := &Accounts{failed: make(map[string][]time.Time), failedSweep: now}
	for i := 0; i < maxLoginPeers; i++ {
		a.failed[fmt.Sprint(i)] = []time.Time{now}
	}
	a.failed["0"] = make([]time.Time, 10)
	for i := range a.failed["0"] {
		a.failed["0"][i] = now
	}
	if a.allowLoginPeerLocked("new", now) || a.allowLoginPeerLocked("0", now) {
		t.Fatal("full rate limit table bypassed")
	}
	if len(a.failed) != maxLoginPeers {
		t.Fatal("rate-limit entries were evicted or grew")
	}
	if !a.allowLoginPeerLocked("new", now.Add(16*time.Minute)) || len(a.failed) != 1 {
		t.Fatal("expired rate-limit entries not collected")
	}
}

func TestUserTokenVaultRejectsUnsafeFiles(t *testing.T) {
	root := t.TempDir()
	s, err := newStorage(root, testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	token := boosty.Tokens{AccessToken: "fixture-token"}
	if err := s.saveUserTokens("alice", token); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "vault", "alice.enc")
	if got, err := s.loadUserTokens("alice"); err != nil || got.AccessToken != token.AccessToken {
		t.Fatalf("valid vault failed: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadUserTokens("alice"); err == nil {
		t.Fatal("world-readable vault accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "private"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadUserTokens("alice"); err == nil {
		t.Fatal("symlinked vault accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := s.saveUserTokens("alice", boosty.Tokens{AccessToken: strings.Repeat("x", maxUserTokenVaultBytes)}); err == nil {
		t.Fatal("oversized token accepted")
	}
	if err := os.WriteFile(path, make([]byte, maxUserTokenVaultBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadUserTokens("alice"); err == nil {
		t.Fatal("oversized vault accepted")
	}
}

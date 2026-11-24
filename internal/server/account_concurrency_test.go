package server

import (
	"testing"
	"time"
)

func TestPasswordVerificationDoesNotHoldAccountLock(t *testing.T) {
	a := &Accounts{data: accountFile{Accounts: map[string]Account{
		"alice": {ID: "alice", Username: "alice", Password: "fixture"},
	}}}
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := a.authenticateWith("alice", "long-password", "peer", func(_, _ string) bool {
			close(started)
			<-release
			return true
		})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("verification never started")
	}
	changed := make(chan struct{})
	go func() {
		a.mu.Lock()
		u := a.data.Accounts["alice"]
		u.Password = "changed"
		a.data.Accounts["alice"] = u
		a.mu.Unlock()
		close(changed)
	}()
	select {
	case <-changed:
		close(release)
	case <-time.After(5 * time.Second):
		close(release)
		<-done
		t.Fatal("account lock held during password verification")
	}
	if err := <-done; err == nil {
		t.Fatal("stale password accepted after account changed")
	}
}

func TestPasswordWorkAdmissionBounded(t *testing.T) {
	if !tryPasswordWork() {
		t.Fatal("first password slot unavailable")
	}
	defer releasePasswordWork()
	if !tryPasswordWork() {
		t.Fatal("second password slot unavailable")
	}
	defer releasePasswordWork()
	if tryPasswordWork() {
		releasePasswordWork()
		t.Fatal("unbounded password work")
	}
}

package boosty

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshDoesNotRestoreReplacedCredentials(t *testing.T) {
	for _, mode := range []string{"logout", "same-refresh-token"} {
		t.Run(mode, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				<-release
				_, _ = w.Write([]byte(`{"access_token":"old-renewed","refresh_token":"old-next","expires_in":60}`))
			}))
			defer api.Close()
			old := Tokens{AccessToken: "old", RefreshToken: "refresh", DeviceID: "device"}
			c := New(old, nil)
			c.api, c.apiBase = api.Client(), api.URL
			result := make(chan error, 1)
			go func() { result <- c.refresh(context.Background(), old, 0) }()
			<-started
			next := Tokens{}
			if mode == "same-refresh-token" {
				next = Tokens{AccessToken: "new", RefreshToken: "refresh", DeviceID: "other-device"}
			}
			set := make(chan error, 1)
			go func() { set <- c.SetTokens(next) }()
			select {
			case err := <-set:
				close(release)
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				close(release)
				t.Fatal("network refresh blocked credential replacement")
			}
			if err := <-result; !errors.Is(err, errCredentialsChanged) || c.Snapshot() != next {
				t.Fatalf("stale refresh committed: %v", err)
			}
		})
	}
}

func TestConcurrentRefreshUsesSingleRequest(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write([]byte(`{"access_token":"new","refresh_token":"next","expires_in":60}`))
	}))
	defer api.Close()
	old := Tokens{AccessToken: "old", RefreshToken: "refresh", DeviceID: "device"}
	c := New(old, nil)
	c.api, c.apiBase = api.Client(), api.URL
	results := make(chan error, 8)
	for i := 0; i < cap(results); i++ {
		go func() { results <- c.refresh(context.Background(), old, 0) }()
	}
	<-started
	close(release)
	for i := 0; i < cap(results); i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 || c.Snapshot().AccessToken != "new" {
		t.Fatalf("duplicate refresh requests: %d", calls.Load())
	}
}

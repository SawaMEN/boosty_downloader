package server

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The gates are bounded even if archives are continually created or removed.
// Different jobs for the same physical archive serialize against each other.
func TestArchiveGatesByPhysicalPostAndBounded(t *testing.T) {
	var gates archiveGates
	first := Job{OwnerID: "owner", Author: "creator", PostID: "post-1", ID: "first"}
	alias := first
	alias.ID = "another-task-same-post"
	other := first
	for n := 2; ; n++ {
		other.PostID = fmt.Sprintf("post-%d", n)
		if gates.gate(other) != gates.gate(first) {
			break
		}
		if n > 1000 {
			t.Fatal("archive lock sharding failed")
		}
	}
	unlockRead, ready := gates.tryRead(first)
	if !ready {
		t.Fatal("first reader should acquire the gate")
	}
	if unlockWrite, ok := gates.tryWrite(alias); ok {
		unlockWrite()
		t.Fatal("another task for the same archive bypassed the reader")
	}
	unlockOther, ready := gates.tryWrite(other)
	if !ready {
		t.Fatal("unrelated archive was blocked by a busy gate")
	}
	unlockOther()
	unlockRead()
	unlockWrite, ready := gates.tryWrite(alias)
	if !ready {
		t.Fatal("writer could not acquire the gate after readers completed")
	}
	if unlock, ok := gates.tryRead(first); ok {
		unlock()
		t.Fatal("reader entered while the archive was being modified")
	}
	unlockWrite()
}

type pauseZIPOnMedia struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *pauseZIPOnMedia) Write(p []byte) (int, error) {
	if bytes.Contains(p, bytes.Repeat([]byte{'B'}, 64)) {
		w.once.Do(func() {
			close(w.entered)
			<-w.release
		})
	}
	return w.ResponseRecorder.Write(p)
}

func TestRestoreCannotReplaceLiveArchiveDuringZIP(t *testing.T) {
	oldMedia := bytes.Repeat([]byte{'A'}, 256<<10)
	newMedia := bytes.Repeat([]byte{'B'}, len(oldMedia))
	app, cookie, clip, zipPath, man := hashedZIPFixture(t, oldMedia)
	id := strings.TrimSuffix(strings.TrimPrefix(zipPath, "/api/jobs/"), "/zip")
	job, ok := app.store.get(id)
	if !ok {
		t.Fatal("archive fixture job disappeared")
	}
	revision, err := app.store.saveArchiveRevision(job)
	if err != nil || !validRevisionID(revision) {
		t.Fatalf("cannot seed historical revision: %s %v", revision, err)
	}
	// The revision has a pinned old inode. Publish new media with a rename,
	// never with an in-place write that would corrupt its hardlinked history.
	if err := writeAtomic(clip, newMedia); err != nil {
		t.Fatal(err)
	}
	man.Title = "New version"
	man.Assets[0].SHA256 = fmt.Sprintf("%x", sha256.Sum256(newMedia))
	if err := writeJSON(filepath.Join(filepath.Dir(filepath.Dir(clip)), "manifest.json"), man); err != nil {
		t.Fatal(err)
	}
	recorder := &pauseZIPOnMedia{
		ResponseRecorder: httptest.NewRecorder(),
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.Handler().ServeHTTP(recorder, reqAs("GET", zipPath, "", cookie))
	}()
	select {
	case <-recorder.entered:
	case <-time.After(15 * time.Second):
		close(recorder.release)
		<-done
		t.Fatal("ZIP never entered the media stream")
	}
	restorePath := "/api/jobs/" + id + "/revisions/" + revision + "/restore"
	refused := httptest.NewRecorder()
	app.Handler().ServeHTTP(refused, reqAs("POST", restorePath, "", cookie))
	if refused.Code != http.StatusConflict {
		close(recorder.release)
		<-done
		t.Fatalf("restore was allowed during a live ZIP: HTTP %d", refused.Code)
	}
	// A read-only request can still access the same immutable version.
	parallel := testArchiveResponse(t, app, cookie, "GET",
		"/api/jobs/"+id+"/manifest", "", http.StatusOK)
	if !bytes.Contains(parallel, []byte("New version")) {
		close(recorder.release)
		<-done
		t.Fatal("parallel reader observed another archive version")
	}
	close(recorder.release)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("ZIP failed to finish after the restore request was rejected")
	}
	if got := zipMediaPayload(t, recorder.Body.Bytes()); !bytes.Equal(got, newMedia) {
		t.Fatal("ZIP mixed live and historical media")
	}
	testArchiveResponse(t, app, cookie, "POST", restorePath, "", http.StatusOK)
	raw := testArchiveResponse(t, app, cookie, "GET", zipPath, "", http.StatusOK)
	if got := zipMediaPayload(t, raw); !bytes.Equal(got, oldMedia) {
		t.Fatal("restored revision was not exported after the previous ZIP finished")
	}
}

func TestArchiveReadersRejectInProgressRefresh(t *testing.T) {
	payload := bytes.Repeat([]byte{'A'}, 32<<10)
	app, cookie, _, zipPath, _ := hashedZIPFixture(t, payload)
	id := strings.TrimSuffix(strings.TrimPrefix(zipPath, "/api/jobs/"), "/zip")
	job, ok := app.store.get(id)
	if !ok {
		t.Fatal("missing archive job")
	}
	rev, err := app.store.saveArchiveRevision(job)
	if err != nil || !validRevisionID(rev) {
		t.Fatalf("failed to seed revision: %s %v", rev, err)
	}
	unlock := app.archiveGates.write(job)
	checks := []struct {
		method string
		path   string
	}{
		{"GET", zipPath},
		{"GET", "/api/jobs/" + id + "/manifest"},
		{"GET", "/library/" + id + "/"},
		{"GET", "/library/" + id + "/media/video.bin"},
		{"GET", "/library/" + id + "/revisions/" + rev + "/"},
		{"GET", "/api/jobs/" + id + "/revisions"},
		{"POST", "/api/jobs/" + id + "/verify"},
		{"POST", "/api/jobs/" + id + "/revisions/" + rev + "/restore"},
	}
	for _, item := range checks {
		rec := httptest.NewRecorder()
		app.Handler().ServeHTTP(rec, reqAs(item.method, item.path, "", cookie))
		if rec.Code != http.StatusConflict {
			unlock()
			t.Fatalf("%s %s: expected a prompt 409 while writer is active, got %d", item.method, item.path, rec.Code)
		}
	}
	unlock()
	testArchiveResponse(t, app, cookie, "GET", zipPath, "", http.StatusOK)
	testArchiveResponse(t, app, cookie, "GET", "/library/"+id+"/", "", http.StatusOK)
}

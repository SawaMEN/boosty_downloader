package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Removing a job from the library revokes future access immediately, but must
// not unlink an archive while an already-authorized ZIP response is streaming.
func TestPurgeTrashedArchiveWaitsForActiveZIP(t *testing.T) {
	media := bytes.Repeat([]byte{'B'}, 256<<10)
	app, owner, clip, zipPath, _ := hashedZIPFixture(t, media)
	id := strings.TrimSuffix(strings.TrimPrefix(zipPath, "/api/jobs/"), "/zip")
	base := filepath.Dir(filepath.Dir(clip))
	job, exists := app.store.get(id)
	if !exists {
		t.Fatal("missing archive fixture")
	}
	revisionID, err := app.store.saveArchiveRevision(job)
	if err != nil || !validRevisionID(revisionID) {
		t.Fatalf("cannot create archive revision: %s %v", revisionID, err)
	}

	paused := &pauseZIPOnMedia{
		ResponseRecorder: httptest.NewRecorder(),
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		app.Handler().ServeHTTP(paused, reqAs("GET", zipPath, "", owner))
	}()
	select {
	case <-paused.entered:
	case <-time.After(15 * time.Second):
		close(paused.release)
		<-finished
		t.Fatal("ZIP never began streaming media")
	}
	// Trash only changes metadata, not the bytes the existing stream uses.
	testArchiveResponse(t, app, owner, "DELETE", "/api/jobs/"+id, "", http.StatusOK)
	denied := httptest.NewRecorder()
	app.Handler().ServeHTTP(denied, reqAs("DELETE", "/api/trash/"+id, "", owner))
	if denied.Code != http.StatusConflict {
		close(paused.release)
		<-finished
		t.Fatalf("purge while ZIP is active returned %d, expected 409", denied.Code)
	}
	// Internal Storage consumers must be unable to bypass the same gate,
	// even if they do not go through the App's HTTP/manual purge wrapper.
	if app.archiveGates != app.store.gates {
		close(paused.release)
		<-finished
		t.Fatal("HTTP and Storage are using different archive gates")
	}
	if n, err := app.store.purgeOwned(job.OwnerID, id, ""); n != 0 || err == nil {
		close(paused.release)
		<-finished
		t.Fatalf("direct Storage purge ignored active ZIP: %d %v", n, err)
	}
	if _, err := os.Stat(clip); err != nil {
		close(paused.release)
		<-finished
		t.Fatal("purge damaged live ZIP source", err)
	}
	if _, err := os.Stat(filepath.Join(app.store.revisionsDir(job), revisionID)); err != nil {
		close(paused.release)
		<-finished
		t.Fatal("purge damaged revision while ZIP was active", err)
	}
	close(paused.release)
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("ZIP did not finish after rejected purge")
	}
	if got := zipMediaPayload(t, paused.Body.Bytes()); !bytes.Equal(got, media) {
		t.Fatal("ZIP lost bytes during attempted purge")
	}
	// A fresh stream cannot start after soft deletion and the same purge
	// succeeds as soon as the previously authorized reader releases its gate.
	testArchiveResponse(t, app, owner, "GET", zipPath, "", http.StatusNotFound)
	testArchiveResponse(t, app, owner, "DELETE", "/api/trash/"+id, "", http.StatusOK)
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("purged live archive still exists: %v", err)
	}
	if _, err := os.Stat(app.store.revisionsDir(job)); !os.IsNotExist(err) {
		t.Fatalf("purged revisions still exist: %v", err)
	}
	if _, ok := app.store.trashedJob("admin", id); ok {
		t.Fatal("purged archive still appears in trash")
	}
}

func TestAutomaticTrashSweepSkipsBusyArchiveAndRetries(t *testing.T) {
	app, err := New(Config{
		AdminUser: "admin", AdminPassword: "strong-password-123",
		SecretKey: testKey(t), DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	job := trashTestJob(t, app.store, strings.Repeat("f", 24))
	if _, err := app.store.removeOwned(job.OwnerID, job.ID, ""); err != nil {
		t.Fatal(err)
	}
	app.store.mu.Lock()
	older := time.Now().UTC().Add(-40 * 24 * time.Hour)
	app.store.jobs[job.ID].DeletedAt = &older
	if err := app.store.saveJobsLocked(); err != nil {
		app.store.mu.Unlock()
		t.Fatal(err)
	}
	app.store.mu.Unlock()
	unlock, ready := app.archiveGates.tryRead(job)
	if !ready {
		t.Fatal("cannot hold active archive gate")
	}
	app.runTrashSweep("manual")
	if _, ok := app.store.trashedJob(job.OwnerID, job.ID); !ok {
		unlock()
		t.Fatal("sweep permanently removed a busy archive")
	}
	path := filepath.Join(app.store.archiveDir(job.OwnerID, job.Author, job.PostID), "index.html")
	if _, err := os.Stat(path); err != nil {
		unlock()
		t.Fatalf("sweep removed busy archive data: %v", err)
	}
	unlock()
	app.runTrashSweep("manual")
	if _, ok := app.store.trashedJob(job.OwnerID, job.ID); ok {
		t.Fatal("sweep failed to retry the expired archive")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("sweep did not permanently remove expired data: %v", err)
	}
	entries, err := app.store.readTrashAudit()
	if err != nil {
		t.Fatal(err)
	}
	foundConflict, foundSuccess := false, false
	for _, entry := range entries {
		if entry.Trigger == "manual" && entry.JobID == job.ID {
			if entry.Failed > 0 {
				foundConflict = true
			}
			if entry.Removed == 1 {
				foundSuccess = true
			}
		}
	}
	if !foundConflict || !foundSuccess {
		t.Fatalf("sweep audit did not record retry: conflict=%t success=%t", foundConflict, foundSuccess)
	}
}

func TestPurgeTrashedArchiveRevalidatesRetentionAndOwnership(t *testing.T) {
	app, err := New(Config{
		AdminUser: "admin", AdminPassword: "strong-password-123",
		SecretKey: testKey(t), DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	job := trashTestJob(t, app.store, strings.Repeat("d", 24))
	if _, err := app.store.removeOwned(job.OwnerID, job.ID, ""); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	if _, err := app.purgeTrashedJob("another-owner", job.ID, time.Time{}); err == nil {
		t.Fatal("non-owner was allowed to purge an archive")
	}
	if _, err := app.purgeTrashedJob(job.OwnerID, job.ID, cutoff); err == nil {
		t.Fatal("freshly trashed archive bypassed retention cutoff")
	}
	if _, ok := app.store.trashedJob(job.OwnerID, job.ID); !ok {
		t.Fatal("failed purge modified trash metadata")
	}
	if _, err := app.store.restoreOwned(job.OwnerID, job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.purgeTrashedJob(job.OwnerID, job.ID, time.Time{}); err == nil {
		t.Fatal("a restored live archive was permanently removed")
	}
	if _, err := os.Stat(app.store.archiveDir(job.OwnerID, job.Author, job.PostID)); err != nil {
		t.Fatal("failed purge removed restored archive", err)
	}
}

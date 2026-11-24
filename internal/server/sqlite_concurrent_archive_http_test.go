package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Pause at the actual response header, after ACL revalidation and opening
// the regular file. No sleeps or scheduling assumptions are needed.
type sqlPausedMediaResponse struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *sqlPausedMediaResponse) WriteHeader(code int) {
	if code == http.StatusOK {
		w.once.Do(func() {
			close(w.entered)
			<-w.release
		})
	}
	w.ResponseRecorder.WriteHeader(code)
}

func TestSQLPrimaryConcurrentArchiveHTTP(t *testing.T) {
	cfg, job, folder := sqlAppFixture(t)
	app, err := newAppMode(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	owner := testLogin(t, app, "admin", "strong-password-123")
	member := testMember(t, app, "concurrentreader")
	api := "/api/jobs/" + job.ID
	media := "/library/" + job.ID + "/media/movie.mp4"
	trash := "/api/trash/" + job.ID
	source := filepath.Join(folder, "media", "movie.mp4")
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := app.store.saveArchiveRevision(job)
	if err != nil || !validRevisionID(revision) {
		t.Fatalf("history fixture: %s %v", revision, err)
	}
	restore := api + "/revisions/" + revision + "/restore"
	share := `{"username":"concurrentreader"}`
	testArchiveResponse(t, app, owner, "POST", api+"/shares", share, 200)
	paused := &sqlPausedMediaResponse{
		ResponseRecorder: httptest.NewRecorder(),
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	done := make(chan struct{})
	var releaseOnce sync.Once
	finish := func() {
		releaseOnce.Do(func() { close(paused.release) })
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("media handler failed to finish")
		}
	}
	defer finish()
	go func() {
		defer close(done)
		app.Handler().ServeHTTP(paused, reqAs("GET", media, "", member))
	}()
	select {
	case <-paused.entered:
	case <-done:
		t.Fatalf("media handler returned before pausing: %d", paused.Code)
	case <-time.After(15 * time.Second):
		t.Fatal("media response did not start")
	}

	testArchiveResponse(t, app, owner, "DELETE", api+"/shares", share, 200)
	testArchiveResponse(t, app, member, "GET", media, "", 404)
	testArchiveResponse(t, app, owner, "POST", restore, "", 409)
	// Trash revokes admission, but cannot physically remove a pinned file.
	testArchiveResponse(t, app, owner, "DELETE", api, "", 200)
	testArchiveResponse(t, app, owner, "GET", media, "", 404)
	testArchiveResponse(t, app, owner, "DELETE", trash, "", 409)
	if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, original) {
		t.Fatal("busy purge changed media", err)
	}
	testArchiveResponse(t, app, owner, "POST", trash+"/restore", "", 200)
	testArchiveResponse(t, app, member, "GET", media, "", 404)
	testArchiveResponse(t, app, owner, "POST", restore, "", 409)
	finish()
	if paused.Code != 200 || !bytes.Equal(paused.Body.Bytes(), original) {
		t.Fatal("already-authorized stream did not retain original bytes")
	}
	testArchiveResponse(t, app, owner, "POST", restore, "", 200)
	if got := testArchiveResponse(t, app, owner, "GET", media, "", 200); !bytes.Equal(got, original) {
		t.Fatal("restored media differs")
	}
	testArchiveResponse(t, app, member, "GET", media, "", 404)
	testArchiveResponse(t, app, owner, "DELETE", api, "", 200)
	testArchiveResponse(t, app, owner, "DELETE", trash, "", 200)
	if _, err := os.Lstat(folder); !os.IsNotExist(err) {
		t.Fatal("purge retained live archive", err)
	}
	if _, err := os.Lstat(app.store.revisionsDir(job)); !os.IsNotExist(err) {
		t.Fatal("purge retained historical archive", err)
	}
	app.Close()
	reopened, err := newAppMode(cfg, true)
	if err != nil {
		t.Fatal("restart after concurrent archive operations", err)
	}
	defer reopened.Close()
	owner = testLogin(t, reopened, "admin", "strong-password-123")
	testArchiveResponse(t, reopened, owner, "GET", media, "", 404)
	if _, ok := reopened.store.trashedJob(job.OwnerID, job.ID); ok {
		t.Fatal("purged archive returned after restart")
	}
	jobs, err := sqliteShadowReadJobs(filepath.Join(cfg.DataDir, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	imports, err := sqliteShadowReadImports(filepath.Join(cfg.DataDir, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.store.primary.verifyJSONParity(context.Background(), jobs, imports); err != nil {
		t.Fatal("SQL/JSON parity", err)
	}
}

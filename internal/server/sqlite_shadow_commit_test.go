package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func sqliteCommitRequest(t *testing.T, app *App, cookie *http.Cookie, want int) {
	t.Helper()
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, reqAs("POST", "/api/admin/sqlite-shadow/commit", "", cookie))
	if rec.Code != want {
		t.Fatalf("SQL commit returned HTTP %d, want %d: %s", rec.Code, want, rec.Body.String())
	}
}

func TestSQLiteOnlineCommitTransactionsJobsImportsAndACL(t *testing.T) {
	app, _ := sqliteShadowApp(t)
	admin := testLogin(t, app, "admin", "strong-password-123")
	ctx := context.Background()
	id := strings.Repeat("1", 24)
	before, ok := app.store.get(id)
	if !ok {
		t.Fatal("missing existing job")
	}
	after := before
	after.Title = "persisted JSON and transactional SQL"
	after.SharedWith = []string{"third"}
	after.SharedForAll = true
	after.AutoSync = true
	if err := app.store.upsert(after); err != nil {
		t.Fatal(err)
	}
	imports := app.store.importsFor("first")
	if len(imports) != 1 {
		t.Fatal("missing imported author")
	}
	updated := imports[0]
	updated.AutoSync = true
	updated.State = "done"
	if err := app.store.updateImport(updated); err != nil {
		t.Fatal(err)
	}
	sqliteCommitRequest(t, app, admin, http.StatusOK)
	got, err := app.store.shadow.meta.job(ctx, "first", id)
	if err != nil || got.Title != after.Title || !got.AutoSync || !got.SharedForAll ||
		len(got.SharedWith) != 1 || got.SharedWith[0] != "third" {
		t.Fatalf("SQL commit lost updated job or grants: %+v %v", got, err)
	}
	gotImport, err := app.store.shadow.meta.authorImport(ctx, "first", updated.ID)
	if err != nil || !gotImport.AutoSync {
		t.Fatalf("SQL commit lost import change: %+v %v", gotImport, err)
	}
	jobs, importsJSON, err := app.store.shadowJSONSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.shadow.meta.verifyJSONParity(ctx, jobs, importsJSON); err != nil {
		t.Fatal("transaction did not preserve full JSON parity:", err)
	}
	status := app.store.sqliteShadowStatus()
	if status.Pending || status.LastError != "" || status.LastSuccess.IsZero() {
		t.Fatalf("SQL checkpoint did not clear metadata lag: %+v", status)
	}
}

func TestSQLiteOnlineCommitAdminOnlyAndDisabledMode(t *testing.T) {
	app, _ := sqliteShadowApp(t)
	member := testMember(t, app, "commitmember")
	sqliteCommitRequest(t, app, member, http.StatusForbidden)
	// A valid admin checkpoint is explicit; merely issuing GET is read only.
	admin := testLogin(t, app, "admin", "strong-password-123")
	sqliteCommitRequest(t, app, admin, http.StatusOK)
}

func TestSQLiteOnlineCommitDisabledWithoutShadow(t *testing.T) {
	t.Setenv("BD_SQLITE_SHADOW", "")
	app, err := New(Config{
		DataDir: t.TempDir(), SecretKey: testKey(t),
		AdminUser: "admin", AdminPassword: "strong-password-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Close)
	admin := testLogin(t, app, "admin", "strong-password-123")
	sqliteCommitRequest(t, app, admin, http.StatusConflict)
}

func TestSQLiteOnlineCommitRejectsForgedGrantWithoutChangingJSON(t *testing.T) {
	app, _ := sqliteShadowApp(t)
	admin := testLogin(t, app, "admin", "strong-password-123")
	ctx := context.Background()
	id := strings.Repeat("1", 24)
	before, ok := app.store.get(id)
	if !ok {
		t.Fatal("missing job")
	}
	if _, err := app.store.shadow.meta.db.Exec(
		"INSERT INTO archive_grants(archive_id,grantee_owner_id,created_at) VALUES(?,?,?)",
		id, "forged", sqliteDate(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	sqliteCommitRequest(t, app, admin, http.StatusServiceUnavailable)
	current, ok := app.store.get(id)
	if !ok || !sqliteShadowEqual(before, current) {
		t.Fatal("failed SQL write mutated authoritative JSON")
	}
	if status := app.store.sqliteShadowStatus(); status.LastError == "" {
		t.Fatal("forged SQL grant was not reported as a degraded index")
	}
	if _, err := app.store.shadow.meta.db.Exec(
		"DELETE FROM archive_grants WHERE archive_id=? AND grantee_owner_id='forged'", id); err != nil {
		t.Fatal(err)
	}
	sqliteCommitRequest(t, app, admin, http.StatusOK)
	if status := app.store.sqliteShadowStatus(); status.LastError != "" {
		t.Fatalf("SQL health did not recover: %+v", status)
	}
	_, err := app.store.shadow.meta.job(ctx, "forged", id)
	if err == nil {
		t.Fatal("unauthorized owner read job after failed SQL commit")
	}
}

func TestSQLiteOnlineCommitBusyWriterReturnsConflict(t *testing.T) {
	app, _ := sqliteShadowApp(t)
	admin := testLogin(t, app, "admin", "strong-password-123")
	app.store.shadow.syncMu.Lock()
	sqliteCommitRequest(t, app, admin, http.StatusConflict)
	app.store.shadow.syncMu.Unlock()
	sqliteCommitRequest(t, app, admin, http.StatusOK)
}

func TestSQLiteOnlineCommitConcurrentJSONChangeIsNotAcknowledged(t *testing.T) {
	app, _ := sqliteShadowApp(t)
	ctx := context.Background()
	id := strings.Repeat("1", 24)
	before, ok := app.store.get(id)
	if !ok {
		t.Fatal("missing job")
	}
	after := before
	after.Title = "updated during SQL commit"
	// Force a successful SQL commit of the OLD JSON snapshot while the JSON
	// writer updates a job before the barrier's generation comparison.
	err := app.store.commitSQLiteMetadataWithHook(ctx, func() {
		if err := app.store.upsert(after); err != nil {
			t.Fatalf("JSON update failed in race hook: %v", err)
		}
	})
	if !errors.Is(err, errSQLiteCommitChanged) {
		t.Fatalf("stale SQL commit was acknowledged: %v", err)
	}
	if !app.store.sqliteShadowStatus().Pending {
		t.Fatal("a newer JSON generation was incorrectly marked mirrored")
	}
	// Re-commit the latest durable JSON. It must become authoritative in SQL
	// without an older snapshot overwriting it out of order.
	if err := app.store.commitSQLiteMetadata(ctx); err != nil {
		t.Fatal(err)
	}
	sqlJob, err := app.store.shadow.meta.job(ctx, before.OwnerID, id)
	if err != nil || sqlJob.Title != after.Title {
		t.Fatalf("newer SQL generation was lost: %+v %v", sqlJob, err)
	}
	if app.store.sqliteShadowStatus().Pending {
		t.Fatal("successful follow-up did not clear the pending generation")
	}
}

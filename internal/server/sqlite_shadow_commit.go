package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"
)

var (
	errSQLiteCommitBusy    = errors.New("SQLite metadata writer is already active")
	errSQLiteCommitChanged = errors.New("authoritative JSON changed during SQLite metadata commit")
)

// commitSQLiteMetadata is an explicit transactional barrier for the *existing*
// authoritative JSON. It is not a SQL-primary cutover. It validates the SQL
// projection, writes jobs/imports/grants in one transaction, reads everything
// back and acknowledges only a generation that has not changed meanwhile.
//
// The shadow background worker uses the same syncMu, so a manual commit cannot
// race a slower full-media reindex or overwrite a newer SQL transaction.
func (s *Storage) commitSQLiteMetadata(ctx context.Context) error {
	return s.commitSQLiteMetadataWithHook(ctx, nil)
}

// The hook exists only to exercise a controlled concurrent JSON commit in
// tests. Normal HTTP callers pass nil and cannot influence the snapshot.
func (s *Storage) commitSQLiteMetadataWithHook(ctx context.Context, afterSnapshot func()) (err error) {
	if s.shadow == nil {
		return errors.New("SQLite shadow is disabled")
	}
	shadow := s.shadow
	if !shadow.syncMu.TryLock() {
		return errSQLiteCommitBusy
	}
	defer shadow.syncMu.Unlock()

	shadow.mu.Lock()
	checkpoint := shadow.changed
	shadow.mu.Unlock()

	defer func() {
		if err == nil || errors.Is(err, errSQLiteCommitChanged) {
			return
		}
		shadow.mu.Lock()
		shadow.lastAttempt = time.Now().UTC()
		shadow.lastError = err.Error()
		shadow.mu.Unlock()
	}()

	jobs, imports, err := s.shadowJSONSnapshot()
	if err != nil {
		return err
	}
	if afterSnapshot != nil {
		afterSnapshot()
	}
	if err = shadow.meta.reconcile(ctx, jobs, imports); err != nil {
		return err
	}
	if err = shadow.meta.verifyJSONParity(ctx, jobs, imports); err != nil {
		return fmt.Errorf("SQL metadata verification after commit failed: %w", err)
	}

	// Lock order matches Storage.saveJobsLocked(): Storage.mu, then shadow.mu.
	// It also creates a linearization point: no new JSON metadata mutation can
	// interleave with the generation comparison and successful acknowledgement.
	s.mu.Lock()
	shadow.mu.Lock()
	shadow.lastAttempt = time.Now().UTC()
	shadow.lastSuccess = shadow.lastAttempt
	shadow.lastError = ""
	if checkpoint > shadow.mirrored {
		shadow.mirrored = checkpoint
	}
	changed := shadow.changed != checkpoint
	shadow.mu.Unlock()
	s.mu.Unlock()

	if changed {
		return errSQLiteCommitChanged
	}
	return nil
}

// verifyJSONParity reads back the complete owner-qualified SQL transaction.
// A forged SQL grant, missing record, inconsistent owner or invalid import
// must not be accepted as a successful checkpoint. It does not read media
// bytes or rewrite the authoritative on-disk JSON.
func (m *sqliteMetadata) verifyJSONParity(ctx context.Context, jobs map[string]Job, imports map[string]AuthorImport) error {
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actualJobs, err := sqliteShadowSQLJobs(ctx, tx)
	if err != nil {
		return err
	}
	actualImports, err := sqliteShadowSQLImports(ctx, tx)
	if err != nil {
		return err
	}
	if !sqliteShadowEqual(jobs, actualJobs) || !sqliteShadowEqual(imports, actualImports) {
		return errors.New("SQLite jobs or imports differ from the authoritative JSON snapshot")
	}
	return tx.Commit()
}

// Only administrators can request a consistent SQL metadata checkpoint.
// It uses a prompt conflict for an active background SQL writer rather than
// blocking an HTTP connection behind potentially huge media indexing.
func (a *App) adminCommitSQLiteShadow(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		fail(w, http.StatusForbidden, "только администратор")
		return
	}
	if a.store.shadow == nil {
		fail(w, http.StatusConflict, "SQLite shadow отключён")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	err := a.store.commitSQLiteMetadata(ctx)
	switch {
	case errors.Is(err, errSQLiteCommitBusy), errors.Is(err, errSQLiteCommitChanged):
		fail(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		fail(w, http.StatusServiceUnavailable, "не удалось зафиксировать SQL-проекцию: "+err.Error())
		return
	}
	status := a.store.sqliteShadowStatus()
	reply(w, http.StatusOK, map[string]any{
		"ok": true, "authoritative": "json", "sqlitePrimary": false,
		"metadataPending": status.Pending, "lastSuccess": status.LastSuccess,
	})
}

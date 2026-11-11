package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// sqliteShadow is a non-authoritative projection. JSON remains the ONLY source
// for HTTP authorization, background scheduling and filesystem operations.
// A failed projection never rolls back a successfully persisted JSON change.
// It is repaired on the next wake-up or restart; never treat it as a backup.
type sqliteShadow struct {
	meta        *sqliteMetadata
	wake        chan struct{}
	mu          sync.Mutex
	lastAttempt time.Time
	lastSuccess time.Time
	lastError   string
}

type sqliteShadowStatus struct {
	LastAttempt time.Time
	LastSuccess time.Time
	LastError   string
}

func (s *Storage) notifySQLiteShadowLocked() {
	if s.shadow == nil {
		return
	}
	select {
	case s.shadow.wake <- struct{}{}:
	default:
	}
}

func (s *Storage) sqliteShadowStatus() sqliteShadowStatus {
	if s.shadow == nil {
		return sqliteShadowStatus{}
	}
	s.shadow.mu.Lock()
	defer s.shadow.mu.Unlock()
	return sqliteShadowStatus{
		LastAttempt: s.shadow.lastAttempt,
		LastSuccess: s.shadow.lastSuccess,
		LastError:   s.shadow.lastError,
	}
}

// shadowJSONSnapshot reads authoritative PERSISTED bytes, not mutable in-memory
// Job pointers. Storage.mu prevents interleaving with any same-process write,
// including the two-file journal. A second writer process is unsupported.
func (s *Storage) shadowJSONSnapshot() (map[string]Job, map[string]AuthorImport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := recoverMetadataPair(s.root); err != nil {
		return nil, nil, err
	}
	jobs, err := sqliteShadowReadJobs(filepath.Join(s.root, "jobs.json"))
	if err != nil {
		return nil, nil, err
	}
	imports, err := sqliteShadowReadImports(filepath.Join(s.root, "imports.json"))
	return jobs, imports, err
}

func sqliteShadowReadJobs(path string) (map[string]Job, error) {
	_, raw, err := sqliteSourceJSON(path)
	if err != nil {
		return nil, err
	}
	var records map[string]Job
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("invalid authoritative jobs.json: %w", err)
	}
	if records == nil {
		return nil, errors.New("jobs.json cannot contain null")
	}
	for id, value := range records {
		if id != value.ID {
			return nil, errors.New("jobs.json has inconsistent archive ID")
		}
		if err := validSQLiteArchive(value); err != nil {
			return nil, fmt.Errorf("invalid JSON job %s: %w", id, err)
		}
	}
	return records, nil
}

func sqliteShadowReadImports(path string) (map[string]AuthorImport, error) {
	_, raw, err := sqliteSourceJSON(path)
	if err != nil {
		return nil, err
	}
	var records map[string]AuthorImport
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("invalid authoritative imports.json: %w", err)
	}
	if records == nil {
		return nil, errors.New("imports.json cannot contain null")
	}
	for id, value := range records {
		if id != value.ID {
			return nil, errors.New("imports.json has inconsistent ID")
		}
		if err := validSQLiteImport(value); err != nil {
			return nil, fmt.Errorf("invalid JSON author import %s: %w", id, err)
		}
	}
	return records, nil
}

func (s *Storage) syncSQLiteShadow(ctx context.Context) error {
	if s.shadow == nil {
		return errors.New("SQLite shadow not enabled")
	}
	jobs, imports, err := s.shadowJSONSnapshot()
	if err == nil {
		err = s.shadow.meta.reconcile(ctx, jobs, imports)
	}
	s.shadow.mu.Lock()
	s.shadow.lastAttempt = time.Now().UTC()
	if err == nil {
		s.shadow.lastSuccess = s.shadow.lastAttempt
		s.shadow.lastError = ""
	} else {
		s.shadow.lastError = err.Error()
	}
	s.shadow.mu.Unlock()
	return err
}

func (s *Storage) sqliteShadowLoop(ctx context.Context) {
	// A periodic sweep recovers missed signals and transient SQLite failures.
	// Signal channel is bounded (one pending item), so a hot download queue
	// cannot allocate unbounded per-write background tasks.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.shadow.wake:
		case <-ticker.C:
		}
		if err := s.syncSQLiteShadow(ctx); err != nil && ctx.Err() == nil {
			log.Printf("SQLite shadow degraded (JSON remains authoritative): %v", err)
		}
	}
}

// enableSQLiteShadow runs BEFORE workers are launched. An already-exported v1
// SQLite metadata.db is required. Any previous SQL projection drift is either
// reconciled transactionally or startup fails, never silently trusted as ACL.
func (s *Storage) enableSQLiteShadow(ctx context.Context) error {
	if s.shadow != nil {
		return errors.New("SQLite shadow already enabled")
	}
	meta, err := openSQLiteMetadata(filepath.Join(s.root, "metadata.db"))
	if err != nil {
		return fmt.Errorf("SQLite shadow needs an existing migrated metadata.db: %w", err)
	}
	s.shadow = &sqliteShadow{meta: meta, wake: make(chan struct{}, 1)}
	if err := s.syncSQLiteShadow(ctx); err != nil {
		s.shadow = nil
		meta.close()
		return fmt.Errorf("SQLite shadow initial reconciliation failed: %w", err)
	}
	return nil
}

// reconcile projects the currently persisted JSON snapshots into the
// normalized SQL tables in ONE transaction. Existing SQL rows must first
// match their own identity columns and grants; corruption is not overwritten.
// Revisions, media bytes, user accounts and private tokens are not mirrored.
func (m *sqliteMetadata) reconcile(ctx context.Context, jobs map[string]Job, imports map[string]AuthorImport) error {
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stored, err := sqliteShadowSQLJobs(ctx, tx)
	if err != nil {
		return err
	}
	storedImports, err := sqliteShadowSQLImports(ctx, tx)
	if err != nil {
		return err
	}

	for id, want := range jobs {
		have, exists := stored[id]
		if !exists {
			if err := sqliteApplyArchive(ctx, tx, sqliteArchiveChange{After: want}); err != nil {
				return err
			}
		} else if !sqliteShadowEqual(have, want) {
			if err := sqliteApplyArchive(ctx, tx, sqliteArchiveChange{Before: &have, After: want}); err != nil {
				return err
			}
		}
		delete(stored, id)
	}
	for id, previous := range stored {
		// Purge only SQL metadata of jobs already purged from JSON. Never
		// unlink media blobs, CAS objects or filesystem revision snapshots.
		if _, err := tx.ExecContext(ctx, "DELETE FROM owner_archives WHERE archive_id=? AND owner_id=?",
			id, previous.OwnerID); err != nil {
			return err
		}
	}
	for id, want := range imports {
		have, exists := storedImports[id]
		if !exists {
			if err := sqliteApplyImport(ctx, tx, sqliteImportChange{After: want}); err != nil {
				return err
			}
		} else if !sqliteShadowEqual(have, want) {
			if err := sqliteApplyImport(ctx, tx, sqliteImportChange{Before: &have, After: want}); err != nil {
				return err
			}
		}
		delete(storedImports, id)
	}
	for id, old := range storedImports {
		if _, err := tx.ExecContext(ctx, "DELETE FROM author_imports WHERE import_id=? AND owner_id=?",
			id, old.OwnerID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func sqliteShadowEqual(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && sqliteSameJSON(left, right)
}

func sqliteShadowSQLJobs(ctx context.Context, tx *sql.Tx) (map[string]Job, error) {
	grantRows, err := tx.QueryContext(ctx, "SELECT archive_id,grantee_owner_id FROM archive_grants")
	if err != nil {
		return nil, err
	}
	grants := make(map[string]map[string]bool)
	for grantRows.Next() {
		var id, user string
		if err := grantRows.Scan(&id, &user); err != nil {
			grantRows.Close()
			return nil, err
		}
		if grants[id] == nil {
			grants[id] = make(map[string]bool)
		}
		grants[id][user] = true
	}
	if err := grantRows.Err(); err != nil {
		grantRows.Close()
		return nil, err
	}
	grantRows.Close()
	rows, err := tx.QueryContext(ctx, `SELECT a.archive_id,a.owner_id,p.author_slug,p.remote_post_id,
		a.job_state,a.title,a.created_at,a.updated_at,a.archived_at,a.auto_sync,a.shared_for_all,a.deleted_at,a.payload_json
		FROM owner_archives a JOIN source_posts p ON a.post_key=p.post_key`)
	if err != nil {
		return nil, err
	}
	result := make(map[string]Job)
	for rows.Next() {
		var id, owner, author, post, state, title, created, updated, raw string
		var autoSync, sharedForAll int
		var archived, deleted sql.NullString
		if err := rows.Scan(&id, &owner, &author, &post, &state, &title,
			&created, &updated, &archived, &autoSync, &sharedForAll, &deleted, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var job Job
		if err := json.Unmarshal([]byte(raw), &job); err != nil {
			rows.Close()
			return nil, err
		}
		if job.ID != id || job.OwnerID != owner || job.Author != author ||
			job.PostID != post || job.State != state || job.Title != title ||
			!sqliteShadowDateMatches(created, job.CreatedAt) ||
			!sqliteShadowDateMatches(updated, job.UpdatedAt) ||
			!sqliteShadowArchivedMatches(archived, job.ArchiveUpdatedAt) ||
			sqliteBool(job.AutoSync) != autoSync ||
			sqliteBool(job.SharedForAll) != sharedForAll ||
			!sqliteShadowDeletedMatches(deleted, job.DeletedAt) {
			rows.Close()
			return nil, fmt.Errorf("SQLite shadow archive identity or ACL drift: %q", id)
		}
		permissions := make(map[string]bool)
		for _, grantee := range job.SharedWith {
			if permissions[grantee] {
				rows.Close()
				return nil, fmt.Errorf("duplicate SQL shadow grant: %q", id)
			}
			permissions[grantee] = true
		}
		if len(permissions) != len(grants[id]) {
			rows.Close()
			return nil, fmt.Errorf("SQLite shadow grant count mismatch: %q", id)
		}
		for grantee := range permissions {
			if !grants[id][grantee] {
				rows.Close()
				return nil, fmt.Errorf("SQLite shadow grant mismatch: %q", id)
			}
		}
		delete(grants, id)
		result[id] = job
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(grants) != 0 {
		return nil, errors.New("SQLite shadow has grants without owner archives")
	}
	return result, nil
}

func sqliteShadowDeletedMatches(value sql.NullString, deleted *time.Time) bool {
	if !value.Valid {
		return deleted == nil
	}
	if deleted == nil {
		return false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	return err == nil && parsed.Equal(*deleted)
}

func sqliteShadowDateMatches(raw string, want time.Time) bool {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	return err == nil && parsed.Equal(want)
}

func sqliteShadowArchivedMatches(raw sql.NullString, want time.Time) bool {
	if !raw.Valid {
		return want.IsZero()
	}
	return sqliteShadowDateMatches(raw.String, want)
}

func sqliteShadowSQLImports(ctx context.Context, tx *sql.Tx) (map[string]AuthorImport, error) {
	rows, err := tx.QueryContext(ctx,
		"SELECT import_id,owner_id,author_slug,job_state,auto_sync,updated_at,payload_json FROM author_imports")
	if err != nil {
		return nil, err
	}
	result := make(map[string]AuthorImport)
	for rows.Next() {
		var id, owner, author, state, updated, raw string
		var autoSync int
		if err := rows.Scan(&id, &owner, &author, &state, &autoSync, &updated, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		var imp AuthorImport
		if err := json.Unmarshal([]byte(raw), &imp); err != nil {
			rows.Close()
			return nil, err
		}
		if imp.ID != id || imp.OwnerID != owner || imp.Author != author ||
			imp.State != state || sqliteBool(imp.AutoSync) != autoSync ||
			!sqliteShadowDateMatches(updated, imp.UpdatedAt) {
			rows.Close()
			return nil, fmt.Errorf("SQLite shadow import identity mismatch: %q", id)
		}
		result[id] = imp
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	return result, rows.Close()
}

// adminSQLiteShadow reports mirror lag and errors without changing the backend.
// Non-administrators cannot query the SQLite state or obtain its errors.
func (a *App) adminSQLiteShadow(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		fail(w, http.StatusForbidden, "только администратор")
		return
	}
	if a.store.shadow == nil {
		reply(w, http.StatusOK, map[string]any{
			"enabled": false, "authoritative": "json", "status": "disabled",
		})
		return
	}
	state := a.store.sqliteShadowStatus()
	status := "healthy"
	if state.LastError != "" || state.LastSuccess.IsZero() {
		status = "degraded"
	}
	reply(w, http.StatusOK, map[string]any{
		"enabled": true, "authoritative": "json", "status": status,
		"lastAttempt": state.LastAttempt, "lastSuccess": state.LastSuccess,
		"lastError": state.LastError,
	})
}

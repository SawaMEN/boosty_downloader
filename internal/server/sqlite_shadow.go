package server

import (
	"bytes"
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
	meta             *sqliteMetadata
	wake             chan struct{}
	syncMu           sync.Mutex // serializes snapshots and SQL commits to prevent stale overwrites
	mu               sync.Mutex
	lastAttempt      time.Time
	lastSuccess      time.Time
	lastError        string
	changed          uint64
	mirrored         uint64
	mediaEnabled     bool
	mediaChanged     uint64
	mediaMirrored    uint64
	mediaError       string
	mediaLastSuccess time.Time
}

type sqliteShadowStatus struct {
	LastAttempt      time.Time
	LastSuccess      time.Time
	LastError        string
	Pending          bool
	MediaEnabled     bool
	MediaPending     bool
	MediaError       string
	MediaLastSuccess time.Time
}

func (s *Storage) notifySQLiteShadowLocked() {
	if s.shadow == nil {
		return
	}
	s.shadow.mu.Lock()
	s.shadow.changed++
	s.shadow.mu.Unlock()
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
		LastAttempt:      s.shadow.lastAttempt,
		LastSuccess:      s.shadow.lastSuccess,
		LastError:        s.shadow.lastError,
		Pending:          s.shadow.changed != s.shadow.mirrored,
		MediaEnabled:     s.shadow.mediaEnabled,
		MediaPending:     s.shadow.mediaEnabled && s.shadow.mediaChanged != s.shadow.mediaMirrored,
		MediaError:       s.shadow.mediaError,
		MediaLastSuccess: s.shadow.mediaLastSuccess,
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
	// Do not allow an older JSON snapshot to commit AFTER a newer one.
	// This also guards explicit sync calls racing with the background worker.
	s.shadow.syncMu.Lock()
	defer s.shadow.syncMu.Unlock()
	s.shadow.mu.Lock()
	checkpoint := s.shadow.changed
	s.shadow.mu.Unlock()
	jobs, imports, err := s.shadowJSONSnapshot()
	if err == nil {
		err = s.shadow.meta.reconcile(ctx, jobs, imports)
	}
	if err == nil {
		attempted, mediaErr := s.syncSQLiteMedia(ctx, jobs, imports)
		if attempted {
			s.shadow.mu.Lock()
			if mediaErr == nil {
				s.shadow.mediaError = ""
				s.shadow.mediaLastSuccess = time.Now().UTC()
			} else {
				s.shadow.mediaError = mediaErr.Error()
			}
			s.shadow.mu.Unlock()
			if mediaErr != nil {
				err = fmt.Errorf("SQLite media shadow: %w", mediaErr)
			}
		}
		// A failed or busy media mirror never undoes authoritative JSON.
		// Its error is reported to the admin and background logs for repair.
	}
	s.shadow.mu.Lock()
	s.shadow.lastAttempt = time.Now().UTC()
	if err == nil {
		s.shadow.lastSuccess = s.shadow.lastAttempt
		s.shadow.lastError = ""
		if checkpoint > s.shadow.mirrored {
			s.shadow.mirrored = checkpoint
		}
	} else if checkpoint >= s.shadow.mirrored {
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
			// Coalesce rapid progress writes without creating a goroutine per
			// event; the 1-element wake channel remains bounded.
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
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
	// Do not block HTTP startup by hashing all media. The first media pass is
	// triggered after workers are launched and only when explicitly requested.
	if os.Getenv("BD_SQLITE_MEDIA_SHADOW") == "1" {
		s.shadow.mu.Lock()
		s.shadow.mediaEnabled = true
		s.shadow.mediaChanged = 1
		s.shadow.mu.Unlock()
		select {
		case s.shadow.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

// reconcile projects the currently persisted JSON snapshots into the
// normalized SQL tables in ONE transaction. Existing SQL rows must first
// match their own identity columns and grants; corruption is not overwritten.
// Revisions, media bytes, user accounts and private tokens are not mirrored.
func (m *sqliteMetadata) reconcile(ctx context.Context, jobs map[string]Job, imports map[string]AuthorImport) error {
	return m.reconcileWithExtra(ctx, jobs, imports, nil)
}

func (m *sqliteMetadata) reconcileWithExtra(ctx context.Context, jobs map[string]Job, imports map[string]AuthorImport, extra func(context.Context, *sql.Tx) error) error {
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
	if extra != nil {
		if err := extra(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func sqliteShadowEqual(a, b any) bool {
	if sqliteMetadataEqualValidated(a, b) {
		return true
	}
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}
	// Equal successful Marshal outputs already prove JSON equality.
	return bytes.Equal(left, right) || sqliteSameJSON(left, right)
}

func sqliteShadowSQLJobs(ctx context.Context, tx *sql.Tx) (map[string]Job, error) {
	result := make(map[string]Job)
	err := sqliteShadowVisitJobs(ctx, tx, func(id string, job Job) error { result[id] = job; return nil })
	if err != nil {
		return nil, err
	}
	return result, nil
}

func sqliteShadowVisitJobs(ctx context.Context, tx *sql.Tx, visit func(string, Job) error) error {
	grantRows, err := tx.QueryContext(ctx, "SELECT archive_id,grantee_owner_id FROM archive_grants ORDER BY archive_id,grantee_owner_id")
	if err != nil {
		return err
	}
	defer grantRows.Close()
	// Merge two ordered cursors in one SQLite snapshot. Keep only one
	// lookahead grant and the current archive's ACL, not all library grants.
	var grantID, grantUser string
	var hasGrant bool
	advanceGrant := func() error {
		previousID, previousUser := grantID, grantUser
		hasGrant = grantRows.Next()
		if !hasGrant {
			return grantRows.Err()
		}
		if err := grantRows.Scan(&grantID, &grantUser); err != nil {
			return err
		}
		if !safeArchivePart(grantID) || !safeArchivePart(grantUser) ||
			grantID < previousID || (grantID == previousID && grantUser <= previousUser) {
			return errors.New("SQLite shadow has invalid or unordered grants")
		}
		return nil
	}
	if err := advanceGrant(); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.archive_id,a.owner_id,p.author_slug,p.remote_post_id,
		a.job_state,a.title,a.created_at,a.updated_at,a.archived_at,a.auto_sync,a.shared_for_all,a.deleted_at,a.payload_json
		FROM owner_archives a JOIN source_posts p ON a.post_key=p.post_key ORDER BY a.archive_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	previousID := ""
	visited := 0
	// Reuse scan destinations within this cursor instead of allocating
	// their addresses once per row. RawBytes is consumed before Next;
	// decoded Jobs must still be fresh, detached values on every row.
	var id, owner, author, post, state, title, created, updated string
	var autoSync, sharedForAll int
	var archived, deleted sql.NullString
	var raw sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&id, &owner, &author, &post, &state, &title,
			&created, &updated, &archived, &autoSync, &sharedForAll, &deleted, &raw); err != nil {
			rows.Close()
			return err
		}
		var job Job
		if err := json.Unmarshal(raw, &job); err != nil {
			rows.Close()
			return err
		}
		if err := validSQLiteArchive(job); err != nil {
			rows.Close()
			return fmt.Errorf("invalid SQL archive %q: %w", id, err)
		}
		if visited != 0 && id <= previousID {
			return fmt.Errorf("duplicate or unordered SQL archive: %q", id)
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
			return fmt.Errorf("SQLite shadow archive identity or ACL drift: %q", id)
		}
		if hasGrant && grantID < id {
			return errors.New("SQLite shadow has grants without owner archives")
		}
		permissions := newSQLiteGrantMatcher(job.SharedWith)
		for hasGrant && grantID == id {
			if !permissions.consume(grantUser) {
				return fmt.Errorf("SQLite shadow grant mismatch: %q", id)
			}
			if err := advanceGrant(); err != nil {
				return err
			}
		}
		if !permissions.complete() {
			return fmt.Errorf("SQLite shadow grant count mismatch: %q", id)
		}
		previousID = id
		visited++
		if err := visit(id, job); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if hasGrant {
		return errors.New("SQLite shadow has grants without owner archives")
	}
	if err := grantRows.Close(); err != nil {
		return err
	}
	// A JOIN may hide orphan owner_archives rows. Never certify a partial set.
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM owner_archives").Scan(&count); err != nil {
		return err
	}
	if count != visited {
		return errors.New("SQLite shadow has owner archives without source posts")
	}
	return nil
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
	result := make(map[string]AuthorImport)
	err := sqliteShadowVisitImports(ctx, tx, func(id string, imp AuthorImport) error { result[id] = imp; return nil })
	if err != nil {
		return nil, err
	}
	return result, nil
}

func sqliteShadowVisitImports(ctx context.Context, tx *sql.Tx, visit func(string, AuthorImport) error) error {
	rows, err := tx.QueryContext(ctx,
		"SELECT import_id,owner_id,author_slug,job_state,auto_sync,updated_at,payload_json FROM author_imports ORDER BY import_id")
	if err != nil {
		return err
	}
	defer rows.Close()
	previousID := ""
	visited := false
	var id, owner, author, state, updated string
	var autoSync int
	var raw sql.RawBytes
	for rows.Next() {
		if err := rows.Scan(&id, &owner, &author, &state, &autoSync, &updated, &raw); err != nil {
			rows.Close()
			return err
		}
		var imp AuthorImport
		if err := json.Unmarshal(raw, &imp); err != nil {
			rows.Close()
			return err
		}
		if err := validSQLiteImport(imp); err != nil {
			rows.Close()
			return fmt.Errorf("invalid SQL import %q: %w", id, err)
		}
		if visited && id <= previousID {
			return fmt.Errorf("duplicate or unordered SQL import: %q", id)
		}
		if imp.ID != id || imp.OwnerID != owner || imp.Author != author ||
			imp.State != state || sqliteBool(imp.AutoSync) != autoSync ||
			!sqliteShadowDateMatches(updated, imp.UpdatedAt) {
			rows.Close()
			return fmt.Errorf("SQLite shadow import identity mismatch: %q", id)
		}
		previousID = id
		visited = true
		if err := visit(id, imp); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
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
			"catalogReadsEnabled": false,
		})
		return
	}
	state := a.store.sqliteShadowStatus()
	status := "healthy"
	if state.LastError != "" || state.LastSuccess.IsZero() || state.MediaError != "" {
		status = "degraded"
	} else if state.Pending || state.MediaPending {
		status = "pending"
	}
	reply(w, http.StatusOK, map[string]any{
		"enabled": true, "authoritative": "json", "status": status,
		"catalogReadsEnabled": a.catalogSQLReads,
		"lastAttempt":         state.LastAttempt, "lastSuccess": state.LastSuccess,
		"lastError": state.LastError, "pending": state.Pending,
		"mediaEnabled": state.MediaEnabled, "mediaPending": state.MediaPending,
		"mediaError": state.MediaError, "mediaLastSuccess": state.MediaLastSuccess,
	})
}

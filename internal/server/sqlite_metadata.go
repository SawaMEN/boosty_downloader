package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// sqliteMetadata is the transactional metadata repository for a DETACHED
// migrated database. It is deliberately not wired into Storage yet: JSON and
// SQLite cannot both be committed atomically with a filesystem rename.
// Production cutover requires a separate journal/backup and crash-recovery plan.
type sqliteMetadata struct {
	db *sql.DB
}

type sqliteArchiveChange struct {
	Before *Job // nil means new archive; otherwise optimistic compare-and-swap
	After  Job
}

type sqliteImportChange struct {
	Before *AuthorImport // nil means new import
	After  AuthorImport
}

var errSQLiteConflict = errors.New("SQLite metadata changed since it was read")

func openSQLiteMetadata(path string) (*sqliteMetadata, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("SQLite metadata path is not a regular file")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// mode=rw refuses to initialize an empty database accidentally.
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     abs,
		RawQuery: "mode=rw&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	var enabled, versions, version int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil || enabled != 1 {
		db.Close()
		return nil, fmt.Errorf("SQLite foreign keys not enabled: %w", err)
	}
	if err := db.QueryRow("SELECT COUNT(*), COALESCE(MAX(migration_id), 0) FROM schema_migrations").
		Scan(&versions, &version); err != nil || versions != 1 || version != 1 {
		db.Close()
		return nil, fmt.Errorf("unsupported SQLite schema: %w", err)
	}
	return &sqliteMetadata{db: db}, nil
}

func (m *sqliteMetadata) close() error { return m.db.Close() }

func (m *sqliteMetadata) job(ctx context.Context, owner, id string) (Job, error) {
	if !safeArchivePart(owner) || !safeArchivePart(id) {
		return Job{}, errors.New("invalid archive identity")
	}
	var raw string
	err := m.db.QueryRowContext(ctx,
		"SELECT payload_json FROM owner_archives WHERE owner_id=? AND archive_id=? AND deleted_at IS NULL",
		owner, id).Scan(&raw)
	if err != nil {
		return Job{}, err
	}
	var j Job
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		return Job{}, err
	}
	if j.ID != id || j.OwnerID != owner {
		return Job{}, errors.New("SQLite archive payload has inconsistent ownership")
	}
	return j, nil
}

func (m *sqliteMetadata) authorImport(ctx context.Context, owner, id string) (AuthorImport, error) {
	if !safeArchivePart(owner) || !safeArchivePart(id) {
		return AuthorImport{}, errors.New("invalid import identity")
	}
	var raw string
	err := m.db.QueryRowContext(ctx,
		"SELECT payload_json FROM author_imports WHERE owner_id=? AND import_id=?",
		owner, id).Scan(&raw)
	if err != nil {
		return AuthorImport{}, err
	}
	var v AuthorImport
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return AuthorImport{}, err
	}
	if v.ID != id || v.OwnerID != owner {
		return AuthorImport{}, errors.New("SQLite import payload has inconsistent ownership")
	}
	return v, nil
}

func sqliteDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func sqliteNullableDate(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return sqliteDate(t)
}

func sqliteBool(value bool) int {
	if value {
		return 1
	}
	return 0
}

func validSQLiteArchive(j Job) error {
	if !safeArchivePart(j.ID) || !safeArchivePart(j.OwnerID) ||
		!safeArchivePart(j.Author) || !safeArchivePart(j.PostID) {
		return errors.New("invalid archive identity")
	}
	switch j.State {
	case "queued", "running", "done", "partial", "failed":
	default:
		return fmt.Errorf("invalid archive state %q", j.State)
	}
	if j.CreatedAt.IsZero() || j.UpdatedAt.IsZero() {
		return errors.New("archive timestamps are required")
	}
	if j.DeletedAt != nil && (j.AutoSync || j.SharedForAll || len(j.SharedWith) != 0) {
		return errors.New("deleted archives cannot retain active sharing or synchronization")
	}
	seen := make(map[string]bool, len(j.SharedWith))
	for _, member := range j.SharedWith {
		if !safeArchivePart(member) || seen[member] {
			return errors.New("invalid or duplicate archive grant")
		}
		seen[member] = true
	}
	return nil
}

func validSQLiteImport(v AuthorImport) error {
	if !safeArchivePart(v.ID) || !safeArchivePart(v.OwnerID) ||
		!safeArchivePart(v.Author) || v.CreatedAt.IsZero() || v.UpdatedAt.IsZero() {
		return errors.New("invalid import identity or timestamps")
	}
	switch v.State {
	case "queued", "running", "done", "failed", "canceled":
		return nil
	default:
		return fmt.Errorf("invalid import state %q", v.State)
	}
}

// apply atomically updates any combination of archives, grants and imports.
// Every previous JSON payload is compared inside the SAME SQL transaction.
// No deletion of blobs, revisions or archive files is performed by this layer.
// This method is intentionally not called by the running JSON application.
func (m *sqliteMetadata) apply(ctx context.Context, archives []sqliteArchiveChange, imports []sqliteImportChange) (err error) {
	if len(archives) == 0 && len(imports) == 0 {
		return nil
	}
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range archives {
		if err := sqliteApplyArchive(ctx, tx, c); err != nil {
			return err
		}
	}
	for _, c := range imports {
		if err := sqliteApplyImport(ctx, tx, c); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func sqliteCheckBefore(ctx context.Context, tx *sql.Tx, query, id string, before any, creating bool) (bool, error) {
	var raw string
	err := tx.QueryRowContext(ctx, query, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		if !creating {
			return false, errSQLiteConflict
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if creating {
		return true, errSQLiteConflict
	}
	expected, err := json.Marshal(before)
	if err != nil {
		return true, err
	}
	if !sqliteSameJSON([]byte(raw), expected) {
		return true, errSQLiteConflict
	}
	return true, nil
}

func sqliteApplyArchive(ctx context.Context, tx *sql.Tx, c sqliteArchiveChange) error {
	j := c.After
	if err := validSQLiteArchive(j); err != nil {
		return err
	}
	if c.Before != nil && (c.Before.ID != j.ID ||
		c.Before.OwnerID != j.OwnerID || c.Before.Author != j.Author ||
		c.Before.PostID != j.PostID || !c.Before.CreatedAt.Equal(j.CreatedAt)) {
		return errors.New("archive ownership and post identity cannot change")
	}
	found, err := sqliteCheckBefore(ctx, tx,
		"SELECT payload_json FROM owner_archives WHERE archive_id=?", j.ID, c.Before, c.Before == nil)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if found {
		var owner, author, postID string
		err = tx.QueryRowContext(ctx, `SELECT a.owner_id,p.author_slug,p.remote_post_id
			FROM owner_archives a JOIN source_posts p ON p.post_key=a.post_key
			WHERE a.archive_id=?`, j.ID).Scan(&owner, &author, &postID)
		if err != nil {
			return err
		}
		if owner != j.OwnerID || author != j.Author || postID != j.PostID {
			return errSQLiteConflict
		}
	}
	if !found {
		_, err = tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO source_posts (author_slug,remote_post_id,first_seen_at) VALUES (?,?,?)",
			j.Author, j.PostID, sqliteDate(j.CreatedAt))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO owner_archives
			 (archive_id,owner_id,post_key,title,job_state,created_at,updated_at,archived_at,
			  deleted_at,auto_sync,shared_for_all,payload_json)
			SELECT ?,?,post_key,?,?,?,?,?,?,?,?,?
			FROM source_posts WHERE author_slug=? AND remote_post_id=?`,
			j.ID, j.OwnerID, j.Title, j.State, sqliteDate(j.CreatedAt), sqliteDate(j.UpdatedAt),
			sqliteNullableDate(j.ArchiveUpdatedAt), sqliteDeletedAt(j.DeletedAt),
			sqliteBool(j.AutoSync), sqliteBool(j.SharedForAll), string(payload),
			j.Author, j.PostID)
	} else {
		res, updateErr := tx.ExecContext(ctx, `
			UPDATE owner_archives
			SET title=?,job_state=?,updated_at=?,archived_at=?,deleted_at=?,
			    auto_sync=?,shared_for_all=?,payload_json=?
			WHERE archive_id=? AND owner_id=?`,
			j.Title, j.State, sqliteDate(j.UpdatedAt), sqliteNullableDate(j.ArchiveUpdatedAt),
			sqliteDeletedAt(j.DeletedAt), sqliteBool(j.AutoSync), sqliteBool(j.SharedForAll),
			string(payload), j.ID, j.OwnerID)
		if updateErr != nil {
			return updateErr
		}
		var affected int64
		affected, err = res.RowsAffected()
		if err == nil && affected != 1 {
			return errSQLiteConflict
		}
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM archive_grants WHERE archive_id=?", j.ID); err != nil {
		return err
	}
	for _, member := range j.SharedWith {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO archive_grants (archive_id,grantee_owner_id,created_at) VALUES (?,?,?)",
			j.ID, member, sqliteDate(j.UpdatedAt)); err != nil {
			return err
		}
	}
	action := "metadata_update"
	if !found {
		action = "metadata_insert"
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO archive_events (owner_id,archive_id,event_type,created_at)
		VALUES (?,?,?,?)`, j.OwnerID, j.ID, action, sqliteDate(j.UpdatedAt))
	return err
}

func sqliteDeletedAt(t *time.Time) any {
	if t == nil {
		return nil
	}
	return sqliteNullableDate(*t)
}

func sqliteApplyImport(ctx context.Context, tx *sql.Tx, c sqliteImportChange) error {
	v := c.After
	if err := validSQLiteImport(v); err != nil {
		return err
	}
	if c.Before != nil && (c.Before.ID != v.ID ||
		c.Before.OwnerID != v.OwnerID || c.Before.Author != v.Author || !c.Before.CreatedAt.Equal(v.CreatedAt)) {
		return errors.New("import ownership and author cannot change")
	}
	found, err := sqliteCheckBefore(ctx, tx,
		"SELECT payload_json FROM author_imports WHERE import_id=?", v.ID, c.Before, c.Before == nil)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if !found {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO author_imports
			(import_id,owner_id,author_slug,job_state,auto_sync,updated_at,payload_json)
			VALUES(?,?,?,?,?,?,?)`,
			v.ID, v.OwnerID, v.Author, v.State, sqliteBool(v.AutoSync),
			sqliteDate(v.UpdatedAt), string(payload))
		return err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE author_imports
		SET job_state=?,auto_sync=?,updated_at=?,payload_json=?
		WHERE import_id=? AND owner_id=? AND author_slug=?`,
		v.State, sqliteBool(v.AutoSync), sqliteDate(v.UpdatedAt),
		string(payload), v.ID, v.OwnerID, v.Author)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errSQLiteConflict
	}
	return nil
}

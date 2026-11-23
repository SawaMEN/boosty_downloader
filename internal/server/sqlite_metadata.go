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

// job reads the owner-qualified SQL projection in one snapshot. A SQL index
// column or grant that disagrees with its JSON payload is corruption, not a
// reason to serve a potentially misclassified or over-shared archive.
func (m *sqliteMetadata) job(ctx context.Context, owner, id string) (Job, error) {
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	job, err := sqliteReadJobTx(ctx, tx, owner, id, false)
	if err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, err
	}
	return job, nil
}

// sqliteReadJobTx validates the complete SQL index and grants in the same
// transaction as a future metadata update. Include deleted jobs only when
// verifying a compare-and-swap write (trash/restore), never for public reads.
func sqliteReadJobTx(ctx context.Context, tx *sql.Tx, owner, id string, includeDeleted bool) (Job, error) {
	if !safeArchivePart(owner) || !safeArchivePart(id) {
		return Job{}, errors.New("invalid archive identity")
	}
	var rowID, rowOwner, author, post, state, title, created, updated, raw string
	var autoSync, everyone int
	var archived, deleted sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT a.archive_id,a.owner_id,p.author_slug,p.remote_post_id,
		a.job_state,a.title,a.created_at,a.updated_at,a.archived_at,a.auto_sync,
		a.shared_for_all,a.deleted_at,a.payload_json
		FROM owner_archives a JOIN source_posts p ON a.post_key=p.post_key
		WHERE a.owner_id=? AND a.archive_id=? AND (?=1 OR a.deleted_at IS NULL)`,
		owner, id, sqliteBool(includeDeleted)).Scan(&rowID, &rowOwner, &author, &post, &state, &title,
		&created, &updated, &archived, &autoSync, &everyone, &deleted, &raw)
	if err != nil {
		return Job{}, err
	}
	var j Job
	if err := json.Unmarshal([]byte(raw), &j); err != nil {
		return Job{}, err
	}
	if err := validSQLiteArchive(j); err != nil {
		return Job{}, fmt.Errorf("invalid SQLite archive payload: %w", err)
	}
	if j.ID != rowID || j.OwnerID != rowOwner || j.Author != author ||
		j.PostID != post || j.State != state || j.Title != title ||
		!sqliteShadowDateMatches(created, j.CreatedAt) ||
		!sqliteShadowDateMatches(updated, j.UpdatedAt) ||
		!sqliteShadowArchivedMatches(archived, j.ArchiveUpdatedAt) ||
		sqliteBool(j.AutoSync) != autoSync || sqliteBool(j.SharedForAll) != everyone ||
		!sqliteShadowDeletedMatches(deleted, j.DeletedAt) || (!includeDeleted && j.DeletedAt != nil) {
		return Job{}, errors.New("SQLite archive indexed fields disagree with payload")
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT grantee_owner_id FROM archive_grants WHERE archive_id=?", id)
	if err != nil {
		return Job{}, err
	}
	expected := make(map[string]bool, len(j.SharedWith))
	for _, grantee := range j.SharedWith {
		expected[grantee] = true
	}
	seen := make(map[string]bool, len(expected))
	for rows.Next() {
		var grantee string
		if err := rows.Scan(&grantee); err != nil {
			rows.Close()
			return Job{}, err
		}
		if !expected[grantee] || seen[grantee] {
			rows.Close()
			return Job{}, errors.New("SQLite archive has unexpected access grants")
		}
		seen[grantee] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Job{}, err
	}
	if err := rows.Close(); err != nil {
		return Job{}, err
	}
	if len(seen) != len(expected) {
		return Job{}, errors.New("SQLite archive has missing access grants")
	}
	return j, nil
}

// authorImport rejects a row when the separately indexed author, state,
// sync flag or modification timestamp disagrees with the immutable SQL row
// payload. This is a prerequisite for using SQL as an authoritative backend.
type sqlitePointReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (m *sqliteMetadata) authorImport(ctx context.Context, owner, id string) (AuthorImport, error) {
	return sqliteReadImport(ctx, m.db, owner, id)
}

func sqliteReadImport(ctx context.Context, q sqlitePointReader, owner, id string) (AuthorImport, error) {
	if !safeArchivePart(owner) || !safeArchivePart(id) {
		return AuthorImport{}, errors.New("invalid import identity")
	}
	var rowID, rowOwner, author, state, updated, raw string
	var autoSync int
	err := q.QueryRowContext(ctx,
		`SELECT import_id,owner_id,author_slug,job_state,auto_sync,updated_at,payload_json
		 FROM author_imports WHERE owner_id=? AND import_id=?`,
		owner, id).Scan(&rowID, &rowOwner, &author, &state, &autoSync, &updated, &raw)
	if err != nil {
		return AuthorImport{}, err
	}
	var v AuthorImport
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return AuthorImport{}, err
	}
	if err := validSQLiteImport(v); err != nil {
		return AuthorImport{}, fmt.Errorf("invalid SQLite import payload: %w", err)
	}
	if v.ID != rowID || v.OwnerID != rowOwner || v.Author != author ||
		v.State != state || sqliteBool(v.AutoSync) != autoSync ||
		!sqliteShadowDateMatches(updated, v.UpdatedAt) {
		return AuthorImport{}, errors.New("SQLite import indexed fields disagree with payload")
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
		// Comparing payload_json alone can silently repair a forged SQL grant
		// or index. Validate the persisted projection BEFORE changing anything.
		current, readErr := sqliteReadJobTx(ctx, tx, j.OwnerID, j.ID, true)
		if readErr != nil || !sqliteShadowEqual(current, *c.Before) {
			return errSQLiteConflict
		}
		// sqliteReadJobTx has already checked owner, post identity and grants.
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
	// Preserve retained grants and their original creation timestamps.
	var previous []string
	if found {previous=c.Before.SharedWith}
	if err:=sqliteApplyGrantDelta(ctx,tx,j.ID,previous,j.SharedWith,sqliteDate(j.UpdatedAt));err!=nil{return err}

	action := "metadata_update"
	if !found {
		action = "metadata_insert"
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO archive_events (owner_id,archive_id,event_type,created_at)
		VALUES (?,?,?,?)`, j.OwnerID, j.ID, action, sqliteDate(j.UpdatedAt))
	return err
}

func sqliteApplyGrantDelta(ctx context.Context,tx *sql.Tx,id string,before,after []string,created string)error{
	old:=make(map[string]bool,len(before))
	for _,member:=range before{old[member]=true}
	for _,member:=range after{
		if old[member]{delete(old,member);continue}
		if _,err:=tx.ExecContext(ctx,"INSERT INTO archive_grants (archive_id,grantee_owner_id,created_at) VALUES (?,?,?)",id,member,created);err!=nil{return err}
	}
	for _,member:=range before{
		if !old[member]{continue}
		result,err:=tx.ExecContext(ctx,"DELETE FROM archive_grants WHERE archive_id=? AND grantee_owner_id=?",id,member)
		if err!=nil{return err}
		n,err:=result.RowsAffected()
		if err!=nil{return err}
		if n!=1{return errSQLiteConflict}
	}
	return nil
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
	if found {
		current, readErr := sqliteReadImport(ctx, tx, v.OwnerID, v.ID)
		if readErr != nil || !sqliteShadowEqual(current, *c.Before) {
			return errSQLiteConflict
		}
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

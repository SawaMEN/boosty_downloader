package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// sqliteDiskRevision represents verified filesystem metadata, not access rights.
// Every revision belongs to one owner_archives row. Media stays on disk.
type sqliteDiskRevision struct {
	id           string
	archiveID    string
	owner        string
	author       string
	path         string
	title        string
	captured     time.Time
	fingerprint  any
	subscription *CapturedSubscription
	current      bool
	assets       []sqliteDiskAsset
}

type sqliteDiskAsset struct {
	path    string
	role    string
	digest  string
	size    int64
	casPath any
}

// ReindexSQLiteRevisionInventory synchronizes the revision/media *index* in an
// existing detached SQLite export, but ONLY for an explicitly stopped server.
// A verified, byte-identical backup of JSON and the original SQLite file is
// mandatory. This function never mutates JSON, ACLs, revisions, or media files.
// It does not enable SQL reads/writes in the application or perform CAS GC.
func ReindexSQLiteRevisionInventory(root, backup string) error {
	if root == "" || backup == "" {
		return errors.New("data directory and verified recovery backup are required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := VerifySQLiteMetadataBackup(backup); err != nil {
		return fmt.Errorf("recovery snapshot verification failed: %w", err)
	}
	if err := sqliteReindexSameBackup(root, backup); err != nil {
		return err
	}
	if err := sqliteReindexNoWAL(root); err != nil {
		return err
	}
	jobs, err := sqliteShadowReadJobs(filepath.Join(root, "jobs.json"))
	if err != nil {
		return err
	}
	imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
	if err != nil {
		return err
	}
	revisions, err := sqliteReindexDisk(root, jobs)
	if err != nil {
		return err
	}

	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		return err
	}
	defer meta.close()
	if err := sqliteAuditDatabaseIntegrity(meta.db); err != nil {
		return err
	}
	if err := sqliteAuditCAS(root, meta.db); err != nil {
		return err
	}
	ctx := context.Background()
	tx, err := meta.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := sqliteApplyRevisionInventory(ctx, tx, revisions, jobs, imports); err != nil {
		return err
	}
	// The authoritative JSON inputs must not change while the expensive media
	// inventory is rebuilt. Do not trust an index made from moving source data.
	for _, name := range []string{"jobs.json", "imports.json"} {
		original, err := backupReadPrivateFile(backup, name)
		if err != nil {
			return err
		}
		now, err := backupReadPrivateFile(root, name)
		if err != nil || !bytes.Equal(now, original) {
			return fmt.Errorf("metadata source changed during reindex: %s", name)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The running service must remain stopped. Checkpoint the offline WAL so
	// immutable read-only audits cannot accidentally ignore committed pages.
	if _, err := meta.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("reindex committed but SQLite checkpoint failed: %w", err)
	}
	return VerifySQLiteSnapshotInventory(root)
}

// sqliteApplyRevisionInventory performs exactly one SQL transaction's revision
// and media changes. The caller owns BeginTx, rollback, commit and all source
// stability checks. This is shared by explicit offline reindex and optional
// non-authoritative media shadow; it does not change files or access rights.
func sqliteApplyRevisionInventory(ctx context.Context, tx *sql.Tx,
	revisions []sqliteDiskRevision, jobs map[string]Job, imports map[string]AuthorImport) error {
	storedJobs, err := sqliteShadowSQLJobs(ctx, tx)
	if err != nil {
		return err
	}
	storedImports, err := sqliteShadowSQLImports(ctx, tx)
	if err != nil {
		return err
	}
	if len(jobs) != len(storedJobs) || len(imports) != len(storedImports) {
		return errors.New("SQLite metadata is not in sync with authoritative JSON")
	}
	for id, want := range jobs {
		if !sqliteShadowEqual(want, storedJobs[id]) {
			return fmt.Errorf("SQLite job metadata or ACL differs: %s", id)
		}
	}
	for id, want := range imports {
		if !sqliteShadowEqual(want, storedImports[id]) {
			return fmt.Errorf("SQLite import metadata differs: %s", id)
		}
	}

	// Check the existing index against immutable historical revisions BEFORE
	// deleting any rows. Old revisions that were pruned from disk may vanish;
	// an indexed history file that changed in place is always an error.
	oldIndex, err := sqliteReindexOldRevisions(ctx, tx, jobs)
	if err != nil {
		return err
	}
	oldAssets, err := sqliteReindexOldAssets(ctx, tx)
	if err != nil {
		return err
	}
	seenRevisions := make(map[string]bool, len(revisions))
	for _, snapshot := range revisions {
		seenRevisions[snapshot.id] = true
		if old, ok := oldIndex[snapshot.id]; ok && !snapshot.current {
			equal, err := sqliteReindexUnchanged(ctx, tx, old, snapshot, oldAssets[snapshot.id])
			if err != nil {
				return err
			}
			if !equal {
				return fmt.Errorf("immutable historical SQLite revision changed: %s", snapshot.id)
			}
		}
	}
	for revisionID := range oldIndex {
		// A completed current snapshot may be replaced, but it cannot simply
		// disappear while the owning archive still exists. Historical snapshots
		// can legitimately disappear after retention pruning.
		if strings.HasPrefix(revisionID, "current:") && !seenRevisions[revisionID] {
			return fmt.Errorf("indexed current archive disappeared from filesystem: %s", revisionID)
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM archive_revisions"); err != nil {
		return err
	}
	for _, snapshot := range revisions {
		var observation any
		if old, ok := oldIndex[snapshot.id]; ok {
			unchanged, err := sqliteReindexUnchanged(ctx, tx, old, snapshot, oldAssets[snapshot.id])
			if err != nil {
				return err
			}
			if unchanged && old.observation.Valid {
				observation = old.observation.Int64
			}
		}
		if observation == nil {
			var err error
			observation, err = sqliteReindexObservation(ctx, tx, snapshot)
			if err != nil {
				return err
			}
		}
		if err := sqliteReindexWriteRevision(ctx, tx, snapshot, observation); err != nil {
			return err
		}
	}
	// JSON purge and retention pruning can remove all SQL references to a
	// digest. Retire only those SQL catalog rows inside the same transaction.
	// Do NOT unlink CAS files: filesystem GC owns physical reclamation.
	if _, err := tx.ExecContext(ctx, `DELETE FROM media_blobs
		WHERE NOT EXISTS (SELECT 1 FROM revision_assets a WHERE a.sha256=media_blobs.sha256)`); err != nil {
		return err
	}
	return nil
}

type sqliteIndexedRevision struct {
	archiveID   string
	path        string
	title       string
	captured    time.Time
	fingerprint sql.NullString
	observation sql.NullInt64
}

func sqliteReindexOldRevisions(ctx context.Context, tx *sql.Tx, jobs map[string]Job) (map[string]sqliteIndexedRevision, error) {
	rows, err := tx.QueryContext(ctx, `SELECT revision_id,archive_id,snapshot_relpath,title,captured_at,is_current,
		source_fingerprint_sha256,subscription_observation_id FROM archive_revisions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]sqliteIndexedRevision)
	for rows.Next() {
		var id, archiveID, path, title, at string
		var isCurrent int
		var fingerprint sql.NullString
		var observation sql.NullInt64
		if err := rows.Scan(&id, &archiveID, &path, &title, &at, &isCurrent,
			&fingerprint, &observation); err != nil {
			return nil, err
		}
		job, ok := jobs[archiveID]
		if !ok {
			return nil, fmt.Errorf("orphan SQLite revision owner: %s", id)
		}
		var expected string
		if isCurrent == 1 {
			if id != "current:"+archiveID {
				return nil, errors.New("invalid SQLite current revision ID")
			}
			expected = filepath.ToSlash(filepath.Join("library", job.OwnerID, job.Author, job.PostID))
		} else if isCurrent == 0 && validRevisionID(id) {
			expected = filepath.ToSlash(filepath.Join("revisions", job.OwnerID, job.Author, job.PostID, id))
		} else {
			return nil, errors.New("invalid historical SQLite revision identity")
		}
		if path != expected {
			return nil, fmt.Errorf("SQLite revision path crosses owner boundary: %s", id)
		}
		date, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, err
		}
		out[id] = sqliteIndexedRevision{
			archiveID: archiveID, path: path, title: title, captured: date,
			fingerprint: fingerprint, observation: observation,
		}
	}
	return out, rows.Err()
}

func sqliteReindexOldAssets(ctx context.Context, tx *sql.Tx) (map[string]map[string]sqliteDiskAsset, error) {
	rows, err := tx.QueryContext(ctx, `SELECT ra.revision_id,ra.asset_relpath,ra.role,ra.sha256,b.byte_length
		FROM revision_assets ra JOIN media_blobs b ON ra.sha256=b.sha256`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]map[string]sqliteDiskAsset)
	for rows.Next() {
		var revision, relative string
		var asset sqliteDiskAsset
		if err := rows.Scan(&revision, &relative, &asset.role, &asset.digest, &asset.size); err != nil {
			return nil, err
		}
		if out[revision] == nil {
			out[revision] = make(map[string]sqliteDiskAsset)
		}
		out[revision][relative] = asset
	}
	return out, rows.Err()
}

func sqliteReindexWriteRevision(ctx context.Context, tx *sql.Tx, revision sqliteDiskRevision, observation any) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO archive_revisions
		(revision_id,archive_id,captured_at,title,source_fingerprint_sha256,
		 snapshot_relpath,subscription_observation_id,asset_count,is_current)
		VALUES (?,?,?,?,?,?,?,?,?)`, revision.id, revision.archiveID,
		sqliteDate(revision.captured), revision.title, revision.fingerprint,
		revision.path, observation, len(revision.assets), sqliteBool(revision.current)); err != nil {
		return err
	}
	for _, asset := range revision.assets {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO media_blobs
			(sha256,byte_length,stored_at,relative_path) VALUES(?,?,?,?)`,
			asset.digest, asset.size, sqliteDate(revision.captured), asset.casPath); err != nil {
			return err
		}
		var size int64
		var storedPath sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT byte_length,relative_path
			FROM media_blobs WHERE sha256=?`, asset.digest).Scan(&size, &storedPath); err != nil {
			return err
		}
		if size != asset.size {
			return fmt.Errorf("media SHA-256 size collision: %s", asset.digest)
		}
		if storedPath.Valid && asset.casPath != nil && storedPath.String != asset.casPath {
			return fmt.Errorf("conflicting CAS path for %s", asset.digest)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO revision_assets
			(revision_id,asset_relpath,role,sha256) VALUES(?,?,?,?)`,
			revision.id, asset.path, asset.role, asset.digest); err != nil {
			return err
		}
	}
	return nil
}

func sqliteReindexSameBackup(root, backup string) error {
	for _, name := range []string{"jobs.json", "imports.json", "metadata.db"} {
		want, _, err := backupFileDigest(backup, name)
		if err != nil {
			return err
		}
		have, _, err := backupFileDigest(root, name)
		if err != nil {
			return err
		}
		if want != have {
			return fmt.Errorf("backup is stale: %s changed after snapshot", name)
		}
	}
	return nil
}

func sqliteReindexNoWAL(root string) error {
	for _, name := range []string{metadataJournalName, "metadata.db-wal"} {
		info, err := os.Lstat(filepath.Join(root, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if name == metadataJournalName || !info.Mode().IsRegular() || info.Size() != 0 {
			return fmt.Errorf("offline reindex blocked by pending journal/WAL: %s", name)
		}
	}
	return nil
}

func sqliteReindexDisk(root string, jobs map[string]Job) ([]sqliteDiskRevision, error) {
	return sqliteReindexDiskContext(context.Background(), root, jobs)
}

func sqliteReindexDiskContext(ctx context.Context, root string, jobs map[string]Job) ([]sqliteDiskRevision, error) {
	keys := make([]string, 0, len(jobs))
	for id := range jobs {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	out := make([]sqliteDiskRevision, 0)
	index := make(map[string]bool)
	ids := make(map[string]bool)
	for _, id := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		job := jobs[id]
		current := []string{"library", job.OwnerID, job.Author, job.PostID}
		_, err := sqliteAuditSafePath(root, append(append([]string{}, current...), "manifest.json"))
		if err == nil {
			revision, err := sqliteReindexReadRevisionContext(ctx, root, job, "current:"+id, current, true)
			if err != nil {
				return nil, err
			}
			out = append(out, revision)
			index[revision.path], ids[revision.id] = true, true
		} else if !os.IsNotExist(err) {
			return nil, err
		} else if job.State == "done" || job.State == "partial" {
			return nil, fmt.Errorf("completed archive lacks current manifest: %s", id)
		}
		history := []string{"revisions", job.OwnerID, job.Author, job.PostID}
		dir, err := sqliteAuditSafePath(root, history)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		stat, err := os.Lstat(dir)
		if err != nil || !stat.IsDir() {
			return nil, fmt.Errorf("unsafe revision root for %s", id)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			revisionID := entry.Name()
			if strings.HasPrefix(revisionID, ".incomplete-") {
				continue
			}
			if !validRevisionID(revisionID) || !entry.IsDir() || ids[revisionID] {
				return nil, fmt.Errorf("invalid or repeated revision ID: %s", revisionID)
			}
			snapshot, err := sqliteReindexReadRevisionContext(ctx, root, job, revisionID,
				append(append([]string{}, history...), revisionID), false)
			if err != nil {
				return nil, err
			}
			ids[revisionID], index[snapshot.path] = true, true
			out = append(out, snapshot)
		}
	}
	for _, part := range []struct {
		root  string
		depth int
	}{{"library", 3}, {"revisions", 4}} {
		if err := sqliteAuditWalkSnapshotTree(root, part.root, part.depth, index); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func sqliteReindexReadRevision(root string, job Job, id string, parts []string, current bool) (sqliteDiskRevision, error) {
	return sqliteReindexReadRevisionContext(context.Background(), root, job, id, parts, current)
}

func sqliteReindexReadRevisionContext(ctx context.Context, root string, job Job, id string, parts []string, current bool) (sqliteDiskRevision, error) {
	if err := ctx.Err(); err != nil {
		return sqliteDiskRevision{}, err
	}
	manifestPath, err := sqliteAuditSafePath(root, append(append([]string{}, parts...), "manifest.json"))
	if err != nil {
		return sqliteDiskRevision{}, err
	}
	htmlPath, err := sqliteAuditSafePath(root, append(append([]string{}, parts...), "index.html"))
	if err != nil {
		return sqliteDiskRevision{}, err
	}
	for _, path := range []string{manifestPath, htmlPath} {
		stat, err := os.Lstat(path)
		if err != nil || !stat.Mode().IsRegular() {
			return sqliteDiskRevision{}, fmt.Errorf("missing or unsafe snapshot entry: %s", path)
		}
	}
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return sqliteDiskRevision{}, err
	}
	var man ArchiveManifest
	if err := json.Unmarshal(manifest, &man); err != nil {
		return sqliteDiskRevision{}, err
	}
	if man.Author != job.Author || man.PostID != job.PostID ||
		man.CapturedAt.IsZero() || len(man.Assets) > 5000 {
		return sqliteDiskRevision{}, fmt.Errorf("snapshot identity or date mismatch: %s", id)
	}
	revision := sqliteDiskRevision{
		id: id, archiveID: job.ID, owner: job.OwnerID, author: job.Author,
		path:  filepath.ToSlash(filepath.Join(parts...)),
		title: man.Title, captured: man.CapturedAt, current: current,
		subscription: man.Subscription, assets: make([]sqliteDiskAsset, 0, len(man.Assets)),
	}
	if sqliteReindexDigestValid(man.ContentSHA256) {
		revision.fingerprint = man.ContentSHA256
	}
	seen := make(map[string]bool, len(man.Assets))
	for _, asset := range man.Assets {
		if err := ctx.Err(); err != nil {
			return sqliteDiskRevision{}, err
		}
		if !safeArchivePart(asset.Filename) || seen[asset.Filename] {
			return sqliteDiskRevision{}, errors.New("unsafe or duplicate archive media filename")
		}
		seen[asset.Filename] = true
		file, err := sqliteAuditSafePath(root,
			append(append([]string{}, parts...), "media", asset.Filename))
		if err != nil {
			return sqliteDiskRevision{}, err
		}
		digest, size, err := sqliteReindexHashFileContext(ctx, file)
		if err != nil {
			return sqliteDiskRevision{}, err
		}
		if (asset.Size != 0 && asset.Size != size) ||
			(asset.SHA256 != "" && asset.SHA256 != digest) {
			return sqliteDiskRevision{}, fmt.Errorf("snapshot media digest or length mismatch: %s", asset.Filename)
		}
		item := sqliteDiskAsset{
			path: filepath.ToSlash(filepath.Join("media", asset.Filename)),
			role: asset.Type, digest: digest, size: size,
		}
		casParts := []string{"blobs", "sha256", digest[:2], digest}
		cas, err := sqliteAuditSafePath(root, casParts)
		if err == nil {
			gotDigest, gotSize, err := sqliteReindexHashFileContext(ctx, cas)
			if err != nil {
				return sqliteDiskRevision{}, err
			}
			if gotDigest != digest || gotSize != size {
				return sqliteDiskRevision{}, fmt.Errorf("CAS media differs from original archive: %s", digest)
			}
			item.casPath = filepath.ToSlash(filepath.Join(casParts...))
		} else if !os.IsNotExist(err) {
			return sqliteDiskRevision{}, err
		}
		revision.assets = append(revision.assets, item)
	}
	return revision, nil
}

func sqliteReindexDigestValid(digest string) bool {
	return len(digest) == 64 && strings.Trim(digest, "0123456789abcdef") == ""
}

func sqliteReindexHashFile(path string) (string, int64, error) {
	return sqliteReindexHashFileContext(context.Background(), path)
}

type sqliteContextReader struct {
	ctx  context.Context
	file io.Reader
}

func (reader sqliteContextReader) Read(p []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.file.Read(p)
}

func sqliteReindexHashFileContext(ctx context.Context, path string) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !before.Mode().IsRegular() {
		return "", 0, errors.New("non-regular archive asset")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", 0, errors.New("archive asset changed before read")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, sqliteContextReader{ctx: ctx, file: f})
	if err != nil {
		return "", 0, err
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, after) ||
		size != opened.Size() || size != after.Size() ||
		!opened.ModTime().Equal(after.ModTime()) {
		return "", 0, errors.New("archive asset changed during hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

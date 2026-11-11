package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// VerifySQLiteSnapshotInventory performs an offline, read-only inventory of
// revisions and real media bytes. It checks BOTH directions: files missing
// from SQL and SQL entries without files. It does not repair SQL or unlink
// anything. Run with archive writers stopped, not in the shadow worker loop.
func VerifySQLiteSnapshotInventory(root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("snapshot audit needs an existing real data directory")
	}
	dbFile := filepath.Join(root, "metadata.db")
	info, err = os.Lstat(dbFile)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("snapshot audit needs a regular SQLite file: %w", err)
	}
	// Immutable read-only mode avoids sidecar writes, but must NEVER ignore an
	// uncheckpointed WAL. Audit only against a stopped, checkpointed database.
	if wal, err := os.Lstat(dbFile + "-wal"); err == nil {
		if !wal.Mode().IsRegular() || wal.Size() != 0 {
			return errors.New("SQLite audit requires a checkpointed database without a nonempty WAL")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	dsn := (&url.URL{Scheme: "file", Path: dbFile, RawQuery: "mode=ro&immutable=1&_pragma=query_only(1)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return err
	}
	if err := sqliteAuditDatabaseIntegrity(db); err != nil {
		return err
	}
	if err := verifySQLiteSnapshots(root, db); err != nil {
		return err
	}
	if err := sqliteAuditInventory(root, db); err != nil {
		return err
	}
	if err := sqliteAuditUnindexedSnapshots(root, db); err != nil {
		return err
	}
	return sqliteAuditCAS(root, db)
}

type sqliteAuditRevision struct {
	archiveID string
	path      string
	captured  string
	title     string
	count     int
}

type sqliteAuditAsset struct {
	role string
	hash string
	size int64
}

func sqliteAuditInventory(root string, db *sql.DB) error {
	all := make(map[string]sqliteAuditRevision)
	rows, err := db.Query(`SELECT revision_id,archive_id,snapshot_relpath,captured_at,title,asset_count
		FROM archive_revisions`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		var item sqliteAuditRevision
		if err := rows.Scan(&id, &item.archiveID, &item.path,
			&item.captured, &item.title, &item.count); err != nil {
			rows.Close()
			return err
		}
		all[id] = item
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	assets := make(map[string]map[string]sqliteAuditAsset)
	rows, err = db.Query(`SELECT ra.revision_id,ra.asset_relpath,ra.role,ra.sha256,b.byte_length
		FROM revision_assets ra JOIN media_blobs b ON b.sha256=ra.sha256`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, rel string
		var item sqliteAuditAsset
		if err := rows.Scan(&id, &rel, &item.role, &item.hash, &item.size); err != nil {
			rows.Close()
			return err
		}
		if assets[id] == nil {
			assets[id] = make(map[string]sqliteAuditAsset)
		}
		assets[id][rel] = item
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	seen := make(map[string]bool, len(all))
	archiveRows, err := db.Query(`SELECT a.archive_id,a.owner_id,p.author_slug,p.remote_post_id,a.job_state
		FROM owner_archives a JOIN source_posts p ON p.post_key=a.post_key`)
	if err != nil {
		return err
	}
	for archiveRows.Next() {
		var id, owner, author, post, state string
		if err := archiveRows.Scan(&id, &owner, &author, &post, &state); err != nil {
			archiveRows.Close()
			return err
		}
		if !safeArchivePart(id) || !safeArchivePart(owner) ||
			!safeArchivePart(author) || !safeArchivePart(post) {
			archiveRows.Close()
			return errors.New("unsafe archive identity in SQLite inventory")
		}
		current := []string{"library", owner, author, post}
		_, err := sqliteAuditSafePath(root, append(current, "manifest.json"))
		if err == nil {
			if err := sqliteAuditSingle(root, id, "current:"+id, current, all, assets, seen, author, post); err != nil {
				archiveRows.Close()
				return err
			}
		} else if !os.IsNotExist(err) {
			archiveRows.Close()
			return err
		} else if state == "done" || state == "partial" {
			archiveRows.Close()
			return fmt.Errorf("completed SQLite archive has no current manifest: %s", id)
		}

		history := []string{"revisions", owner, author, post}
		dir, err := sqliteAuditSafePath(root, history)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			archiveRows.Close()
			return err
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			archiveRows.Close()
			return fmt.Errorf("unsafe revision directory for %s: %w", id, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			archiveRows.Close()
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".incomplete-") {
				continue // unfinished snapshot was never committed
			}
			if !validRevisionID(name) || !entry.IsDir() {
				archiveRows.Close()
				return fmt.Errorf("unexpected historical archive entry: %s", name)
			}
			if err := sqliteAuditSingle(root, id, name, append(history, name),
				all, assets, seen, author, post); err != nil {
				archiveRows.Close()
				return err
			}
		}
	}
	if err := archiveRows.Err(); err != nil {
		archiveRows.Close()
		return err
	}
	archiveRows.Close()
	if len(seen) != len(all) {
		return fmt.Errorf("SQL revision index contains %d entries without corresponding snapshots", len(all)-len(seen))
	}
	return nil
}

// sqliteAuditSafePath rejects symlinks anywhere below the given root.
func sqliteAuditSafePath(root string, parts []string) (string, error) {
	path := root
	for i, part := range parts {
		if !safeArchivePart(part) {
			return "", errors.New("unsafe archive path component")
		}
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symlink inside archive inventory")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", errors.New("non-directory component in archive inventory")
		}
	}
	return path, nil
}

func sqliteAuditSingle(root, jobID, revision string, parts []string,
	index map[string]sqliteAuditRevision, assets map[string]map[string]sqliteAuditAsset,
	seen map[string]bool, author, post string) error {
	record, ok := index[revision]
	expected := filepath.ToSlash(filepath.Join(parts...))
	if !ok || seen[revision] || record.archiveID != jobID || record.path != expected {
		return fmt.Errorf("missing or inconsistent SQL revision: %s", revision)
	}
	base, err := sqliteAuditSafePath(root, parts)
	if err != nil {
		return err
	}
	index, err := sqliteAuditSafePath(root, append(append([]string{}, parts...), "index.html"))
	if err != nil {
		return err
	}
	pageInfo, err := os.Lstat(index)
	if err != nil || !pageInfo.Mode().IsRegular() {
		return fmt.Errorf("revision HTML is missing or unsafe: %s", revision)
	}
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("revision path not a directory: %s", revision)
	}
	manifestPath, err := sqliteAuditSafePath(root, append(append([]string{}, parts...), "manifest.json"))
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var man ArchiveManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return err
	}
	if man.Author != author || man.PostID != post || man.Title != record.title ||
		!sqliteAuditDateMatch(record.captured, man.CapturedAt) ||
		len(man.Assets) != record.count || len(man.Assets) != len(assets[revision]) {
		return fmt.Errorf("snapshot manifest differs from SQL index: %s", revision)
	}
	found := make(map[string]bool, len(man.Assets))
	for _, asset := range man.Assets {
		if !safeArchivePart(asset.Filename) || found[asset.Filename] {
			return fmt.Errorf("unsafe or duplicate media file in revision: %s", revision)
		}
		found[asset.Filename] = true
		rel := filepath.ToSlash(filepath.Join("media", asset.Filename))
		sqlAsset, ok := assets[revision][rel]
		if !ok || asset.Type != sqlAsset.role ||
			(asset.Size != 0 && asset.Size != sqlAsset.size) ||
			(asset.SHA256 != "" && asset.SHA256 != sqlAsset.hash) {
			return fmt.Errorf("SQL media metadata differs: %s/%s", revision, rel)
		}
		file, err := sqliteAuditSafePath(root, append(append([]string{}, parts...), "media", asset.Filename))
		if err != nil {
			return err
		}
		if err := sqliteAuditFileDigest(file, sqlAsset.hash, sqlAsset.size); err != nil {
			return fmt.Errorf("media integrity failed: %s/%s: %w", revision, rel, err)
		}
	}
	seen[revision] = true
	return nil
}

func sqliteAuditDateMatch(value string, date time.Time) bool {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && parsed.Equal(date)
}

func sqliteAuditFileDigest(path, wantHash string, wantSize int64) error {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return errors.New("media file is not regular")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return errors.New("media file changed during open")
	}
	digest := sha256.New()
	size, err := io.Copy(digest, f)
	if err != nil {
		return err
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, after) ||
		before.Size() != size || after.Size() != size ||
		before.ModTime() != after.ModTime() {
		return errors.New("media file changed during hashing")
	}
	if size != wantSize || hex.EncodeToString(digest.Sum(nil)) != wantHash {
		return errors.New("media SHA-256 or size mismatch")
	}
	return nil
}

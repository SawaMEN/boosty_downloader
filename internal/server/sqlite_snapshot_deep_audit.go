package server

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sqliteAuditDatabaseIntegrity validates the actual persisted SQLite structure,
// not only the rows reachable through joins. A missing parent foreign key must
// never be hidden by an inner JOIN during a read-only inventory audit.
func sqliteAuditDatabaseIntegrity(db *sql.DB) error {
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("SQLite integrity check failed: %s", result)
	}
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("SQLite inventory contains orphan foreign keys")
	}
	return rows.Err()
}

// sqliteAuditUnindexedSnapshots checks that every complete on-disk archive
// belongs to a known SQL revision. A forward-only database scan could miss
// private archived files left behind by a failed migration or SQL purge.
func sqliteAuditUnindexedSnapshots(root string, db *sql.DB) error {
	rows, err := db.Query("SELECT snapshot_relpath FROM archive_revisions")
	if err != nil {
		return err
	}
	index := make(map[string]bool)
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			rows.Close()
			return err
		}
		if index[path] {
			rows.Close()
			return fmt.Errorf("duplicate SQL snapshot path: %s", path)
		}
		index[path] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, area := range []struct {
		name  string
		depth int
	}{
		{name: "library", depth: 3},
		{name: "revisions", depth: 4},
	} {
		if err := sqliteAuditWalkSnapshotTree(root, area.name, area.depth, index); err != nil {
			return err
		}
	}
	return nil
}

func sqliteAuditWalkSnapshotTree(root, name string, depth int, indexed map[string]bool) error {
	dir := filepath.Join(root, name)
	stat, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !stat.IsDir() || stat.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe %s inventory directory", name)
	}
	return sqliteAuditWalkSnapshotLevel(dir, name, depth, indexed)
}

func sqliteAuditWalkSnapshotLevel(dir, relative string, depth int, indexed map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		next := filepath.Join(dir, entry.Name())
		info, err := os.Lstat(next)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in snapshot inventory tree: %s", next)
		}
		if depth == 1 && strings.HasPrefix(entry.Name(), ".incomplete-") &&
			strings.HasPrefix(relative, "revisions"+string(os.PathSeparator)) {
			// A historical copy is visible only after its staging directory
			// has been atomically renamed to a valid revision identifier.
			if !info.IsDir() {
				return fmt.Errorf("unsafe incomplete revision stage: %s", next)
			}
			continue
		}
		if !safeArchivePart(entry.Name()) || !info.IsDir() {
			return fmt.Errorf("unexpected snapshot inventory entry: %s", next)
		}
		rel := filepath.Join(relative, entry.Name())
		if depth > 1 {
			if err := sqliteAuditWalkSnapshotLevel(next, rel, depth-1, indexed); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(relative, "revisions"+string(os.PathSeparator)) &&
			!validRevisionID(entry.Name()) {
			return fmt.Errorf("invalid historical revision directory: %s", rel)
		}
		manifestPath := filepath.Join(next, "manifest.json")
		manifest, err := os.Lstat(manifestPath)
		if os.IsNotExist(err) && !strings.HasPrefix(relative, "revisions"+string(os.PathSeparator)) {
			// Queued/running job may own an empty or not-yet-finished directory.
			// It has no committed snapshot to index.
			continue
		}
		if err != nil {
			return err
		}
		if !manifest.Mode().IsRegular() {
			return fmt.Errorf("unsafe manifest in snapshot directory: %s", next)
		}
		if !indexed[filepath.ToSlash(rel)] {
			return fmt.Errorf("on-disk revision is not present in SQLite index: %s", rel)
		}
	}
	return nil
}

// sqliteAuditCAS validates declared content-addressed objects without deleting
// orphan links. NULL relative_path is legitimate when hardlink deduplication is
// disabled. Existing SQL paths must follow the exact private digest layout.
func sqliteAuditCAS(root string, db *sql.DB) error {
	rows, err := db.Query("SELECT sha256,byte_length,relative_path FROM media_blobs")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var digest string
		var size int64
		var relative sql.NullString
		if err := rows.Scan(&digest, &size, &relative); err != nil {
			return err
		}
		if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" || size < 0 {
			return errors.New("invalid SQL media digest or length")
		}
		if !relative.Valid {
			continue
		}
		expected := filepath.ToSlash(filepath.Join("blobs", "sha256", digest[:2], digest))
		if relative.String != expected {
			return fmt.Errorf("SQL CAS path is not canonical for digest %s", digest)
		}
		file, err := sqliteAuditSafePath(root, []string{"blobs", "sha256", digest[:2], digest})
		if err != nil {
			return err
		}
		if err := sqliteAuditFileDigest(file, digest, size); err != nil {
			return fmt.Errorf("CAS file verification failed for %s: %w", digest, err)
		}
	}
	return rows.Err()
}

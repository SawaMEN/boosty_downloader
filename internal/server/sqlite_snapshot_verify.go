package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// verifySQLiteSnapshots independently verifies revision ownership and path
// structure. Database integrity alone cannot detect a revision linked to the
// wrong owner's valid filesystem path. This never exposes raw post payloads.
func verifySQLiteSnapshots(root string, db *sql.DB) error {
	rows, err := db.Query(`
		SELECT r.revision_id,r.archive_id,a.owner_id,p.author_slug,p.remote_post_id,
		       r.snapshot_relpath,r.asset_count,r.is_current,
		       (SELECT COUNT(*) FROM revision_assets ra WHERE ra.revision_id=r.revision_id)
		FROM archive_revisions r
		JOIN owner_archives a ON a.archive_id=r.archive_id
		JOIN source_posts p ON p.post_key=a.post_key`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var revision, jobID, owner, author, postID, relative string
		var declaredAssets, actualAssets, current int
		if err := rows.Scan(&revision, &jobID, &owner, &author, &postID,
			&relative, &declaredAssets, &current, &actualAssets); err != nil {
			rows.Close()
			return err
		}
		if !safeArchivePart(owner) || !safeArchivePart(author) || !safeArchivePart(postID) ||
			!safeArchivePart(jobID) {
			rows.Close()
			return errors.New("unsafe SQLite snapshot identity")
		}
		var expected string
		switch current {
		case 1:
			if revision != "current:"+jobID {
				rows.Close()
				return errors.New("invalid current SQLite revision identifier")
			}
			expected = filepath.ToSlash(filepath.Join("library", owner, author, postID))
		case 0:
			if !validRevisionID(revision) {
				rows.Close()
				return errors.New("invalid historical SQLite revision identifier")
			}
			expected = filepath.ToSlash(filepath.Join("revisions", owner, author, postID, revision))
		default:
			rows.Close()
			return errors.New("invalid SQLite revision state")
		}
		if relative != expected || declaredAssets != actualAssets {
			rows.Close()
			return fmt.Errorf("SQLite archive revision ownership or asset count mismatch: %s", revision)
		}
		dir := root
		parts := []string{"library", owner, author, postID}
		if current == 0 {
			parts = append([]string{"revisions", owner, author, postID}, revision)
		}
		for _, part := range parts {
			dir = filepath.Join(dir, part)
			info, err := os.Lstat(dir)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				rows.Close()
				return fmt.Errorf("SQLite revision directory is missing or unsafe: %s: %w", revision, err)
			}
		}
		file := filepath.Join(dir, "manifest.json")
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() {
			rows.Close()
			return fmt.Errorf("SQLite revision manifest missing or unsafe: %s: %w", revision, err)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			rows.Close()
			return err
		}
		var man ArchiveManifest
		if json.Unmarshal(data, &man) != nil || man.Author != author ||
			man.PostID != postID || len(man.Assets) != declaredAssets {
			rows.Close()
			return fmt.Errorf("SQLite revision manifest differs: %s", revision)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
}

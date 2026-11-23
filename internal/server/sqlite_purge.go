package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Validate every selected archive's indexed current and historical versions
// before staging any folder. Other jobs and owners are not rewritten. The
// filesystem gate and Storage.mu prevent cooperating writers during purge.
func (s *Storage) verifySQLPurgeLocked(jobs map[string]Job, selected map[string]bool, folders map[string]bool) error {
	ctx := context.Background()
	tx, err := s.primary.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	old, err := sqliteReindexOldRevisions(ctx, tx, jobs)
	if err != nil {
		return err
	}
	assets, err := sqliteReindexOldAssets(ctx, tx)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	for id := range selected {
		job, ok := jobs[id]
		if !ok || job.DeletedAt == nil {
			return errors.New("SQL purge requires a trashed archive")
		}
		for _, kind := range []string{"library", "revisions"} {
			base := filepath.Join(s.root, kind, job.OwnerID, job.Author, job.PostID)
			if !folders[base] {
				return errors.New("SQL purge refuses a folder shared by multiple job identities")
			}
			info, err := purgeSafePath(s.root, base)
			if err != nil {
				return err
			}
			if info == nil {
				continue
			}
			parts := []string{kind, job.OwnerID, job.Author, job.PostID}
			if kind == "library" {
				rev, err := sqliteReindexReadRevision(s.root, job, "current:"+id, parts, true)
				if err != nil {
					return err
				}
				row, ok := old[rev.id]
				if !ok {
					return errors.New("SQL purge found unindexed current archive")
				}
				equal, err := sqliteReindexUnchanged(ctx, tx, row, rev, assets[rev.id])
				if err != nil || !equal {
					return errors.New("SQL purge current index mismatch")
				}
				seen[rev.id] = true
				continue
			}
			entries, err := os.ReadDir(base)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				// Do not destroy unresolved or unindexed historical files.
				if !entry.IsDir() || !validRevisionID(entry.Name()) {
					return fmt.Errorf("unexpected SQL purge history entry: %s", entry.Name())
				}
				rev, err := sqliteReindexReadRevision(s.root, job, entry.Name(), append(append([]string{}, parts...), entry.Name()), false)
				if err != nil {
					return err
				}
				row, ok := old[rev.id]
				if !ok {
					return errors.New("SQL purge found unindexed history")
				}
				equal, err := sqliteReindexUnchanged(ctx, tx, row, rev, assets[rev.id])
				if err != nil || !equal {
					return errors.New("SQL purge history index mismatch")
				}
				seen[rev.id] = true
			}
		}
	}
	for id, row := range old {
		if selected[row.archiveID] && !seen[id] {
			return fmt.Errorf("SQL purge indexed revision missing from disk: %s", id)
		}
	}
	return tx.Commit()
}

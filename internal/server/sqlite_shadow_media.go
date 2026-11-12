package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
)

// notifySQLiteMedia marks a durable filesystem transition for optional
// background SQL indexing. It deliberately never blocks archive downloads
// on SQLite work and can be called with or without Storage.mu held.
func (s *Storage) notifySQLiteMedia() {
	if s.shadow == nil {
		return
	}
	s.shadow.mu.Lock()
	if !s.shadow.mediaEnabled {
		s.shadow.mu.Unlock()
		return
	}
	s.shadow.mediaChanged++
	s.shadow.mu.Unlock()
	select {
	case s.shadow.wake <- struct{}{}:
	default:
	}
}

// syncSQLiteMedia runs from the serialized shadow worker after JSON metadata
// reconciliation. A queued/running download makes the filesystem unstable, so
// leave the existing SQL index intact and retain the pending event until idle.
// This is *never* an authorization source or a fallback for missing JSON.
func (s *Storage) syncSQLiteMedia(ctx context.Context, jobs map[string]Job, imports map[string]AuthorImport) (bool, error) {
	s.shadow.mu.Lock()
	checkpoint := s.shadow.changed
	mediaCheckpoint := s.shadow.mediaChanged
	pending := s.shadow.mediaEnabled && mediaCheckpoint != s.shadow.mediaMirrored
	s.shadow.mu.Unlock()
	if !pending {
		return false, nil
	}
	for _, job := range jobs {
		if job.State == "running" || job.State == "queued" {
			return false, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	// Indexing involves SHA-256 of on-disk bytes. It is opt-in, not scheduled
	// for every JSON progress save, and no files are ever modified or removed.
	revisions, err := sqliteReindexDiskContext(ctx, s.root, jobs)
	if err != nil {
		return true, err
	}
	if err := ctx.Err(); err != nil {
		return true, err
	}
	tx, err := s.shadow.meta.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return true, err
	}
	defer tx.Rollback()
	if err := sqliteApplyRevisionInventory(ctx, tx, revisions, jobs, imports); err != nil {
		return true, err
	}

	// Recheck persisted JSON and generation numbers while holding Storage.mu
	// through COMMIT. A simultaneous import, share, retry or download cannot
	// turn this index into a snapshot of an earlier owner's ACL.
	s.mu.Lock()
	defer s.mu.Unlock()
	freshJobs, err := sqliteShadowReadJobs(filepath.Join(s.root, "jobs.json"))
	if err != nil {
		return true, err
	}
	freshImports, err := sqliteShadowReadImports(filepath.Join(s.root, "imports.json"))
	if err != nil {
		return true, err
	}
	if !sqliteShadowEqual(jobs, freshJobs) || !sqliteShadowEqual(imports, freshImports) {
		return true, errors.New("authoritative JSON changed during media shadow; retry")
	}
	s.shadow.mu.Lock()
	changed := s.shadow.changed != checkpoint || s.shadow.mediaChanged != mediaCheckpoint
	s.shadow.mu.Unlock()
	if changed {
		return true, errors.New("media shadow generation advanced during scan; retry")
	}
	if err := tx.Commit(); err != nil {
		return true, fmt.Errorf("media shadow transaction failed: %w", err)
	}
	s.shadow.mu.Lock()
	s.shadow.mediaMirrored = mediaCheckpoint
	s.shadow.mu.Unlock()
	return true, nil
}

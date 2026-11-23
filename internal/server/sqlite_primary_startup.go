package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Called only after file journals have been recovered, while the caller
// owns the data-dir flock. Unlike legacy JSON initialization, no imports
// or jobs image is rewritten before SQL and its mirrors are checked.
func openSQLPrimaryStorageLocked(root string, key []byte, checkpoint func(string) error) (*Storage, error) {
	if err := sqliteHistoryDeleteGuard(root, false); err != nil {
		return nil, err
	}
	// The writable handle is needed to checkpoint committed WAL frames
	// left by SIGKILL. It must never create a missing database.
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, err
	}
	ready := false
	defer func() {
		if !ready {
			_ = meta.close()
		}
	}()
	var busy, frames, done int
	if err := meta.db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &done); err != nil || busy != 0 {
		return nil, fmt.Errorf("SQL startup checkpoint failed: busy=%d: %w", busy, err)
	}
	if err := sqliteReindexNoWAL(root); err != nil {
		return nil, err
	}
	if err := verifyJSONSQLiteAgreement(root); err != nil {
		return nil, fmt.Errorf("SQL startup mirror parity failed: %w", err)
	}
	if err := verifySQLiteSnapshotInventory(root); err != nil {
		return nil, fmt.Errorf("SQL startup archive inventory failed: %w", err)
	}
	jobsRaw, err := videoPublicationReadFile(root, "jobs.json", 64<<20)
	if err != nil {
		return nil, err
	}
	importsRaw, err := videoPublicationReadFile(root, "imports.json", 64<<20)
	if err != nil {
		return nil, err
	}
	mirrorJobs, mirrorImports, err := sqlitePrimaryDecodeImages(jobsRaw, importsRaw)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	tx, err := meta.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	jobs, err := sqliteShadowSQLJobs(ctx, tx)
	if err != nil {
		return nil, err
	}
	imports, err := sqliteShadowSQLImports(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !sqliteShadowEqual(jobs, mirrorJobs) || !sqliteShadowEqual(imports, mirrorImports) {
		return nil, errors.New("SQL startup snapshot differs from recovery mirror")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s := &Storage{root: root, key: key, jobs: make(map[string]*Job), imports: make(map[string]*AuthorImport),
		authors: make(map[string][]string), gates: &archiveGates{}, primary: meta}
	authors, err := videoPublicationReadFile(root, "authors.json", 64<<20)
	if err == nil {
		if err := json.Unmarshal(authors, &s.authors); err != nil {
			return nil, err
		}
		if s.authors == nil {
			s.authors = map[string][]string{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	// An interrupted worker must be requeued by one SQL-authoritative
	// transaction. Preserve ownership, ACL, capture date and other fields.
	changed := false
	for id, job := range jobs {
		if job.State == "running" {
			job.State = "queued"
			job.Error = ""
			jobs[id] = job
			changed = true
		}
	}
	for id, imp := range imports {
		if imp.State == "running" {
			imp.State = "queued"
			imports[id] = imp
			changed = true
		}
	}
	if changed {
		if err := sqlitePrimaryCommitMetadata(root, meta, jobs, imports, &sqlitePrimaryCommitHooks{checkpoint: checkpoint}); err != nil {
			return nil, fmt.Errorf("SQL startup requeue failed: %w", err)
		}
	}
	for id, job := range jobs {
		copy := job
		s.jobs[id] = &copy
	}
	for id, imp := range imports {
		copy := imp
		s.imports[id] = &copy
	}
	if _, _, err := s.sqlitePrimaryReadSnapshotLocked(); err != nil {
		return nil, err
	}
	ready = true
	return s, nil
}

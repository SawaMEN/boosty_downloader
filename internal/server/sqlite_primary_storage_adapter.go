package server

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
)

// saveSQLitePrimaryLocked is the common write adapter for jobs, imports and
// combined ACL/schedule changes. It is called with Storage.mu held. This
// adapter is deliberately NOT enabled by a production configuration yet:
// startup backend selection, all read paths and filesystem side effects need
// a separately audited cutover before live SQL-primary is safe.
func (s *Storage) saveSQLitePrimaryLocked() error {
	if s.primary == nil {
		return errors.New("SQL-first storage adapter is not configured")
	}
	if s.primaryFatal != nil {
		return fmt.Errorf("SQL-first storage was quarantined after an unrecoverable write: %w", s.primaryFatal)
	}
	if s.shadow != nil {
		return errors.New("SQL-first writer cannot share a database with the JSON shadow reconciler")
	}
	jobs := make(map[string]Job, len(s.jobs))
	for id, job := range s.jobs {
		if job == nil {
			return fmt.Errorf("nil job in SQL-first storage: %s", id)
		}
		jobs[id] = *job
	}
	imports := make(map[string]AuthorImport, len(s.imports))
	for id, item := range s.imports {
		if item == nil {
			return fmt.Errorf("nil author import in SQL-first storage: %s", id)
		}
		imports[id] = *item
	}
	if err := sqlitePrimaryCommitMetadata(s.root, s.primary, jobs, imports, nil); err == nil {
		return nil
	} else {
		// A failed JSON fsync or a crash-resistant journal cleanup may have
		// returned an error AFTER SQL committed. The caller will roll back its
		// in-memory pointers if we return an error, so recognize a fully
		// recovered AFTER generation as success instead.
		if recovered := recoverSQLitePrimaryCommitLocked(s.root); recovered != nil {
			s.primaryFatal = fmt.Errorf("SQL/JSON recovery failed after write error %v: %w", err, recovered)
			return s.primaryFatal
		}
		onDiskJobs, readJobsErr := sqliteShadowReadJobs(filepath.Join(s.root, "jobs.json"))
		onDiskImports, readImportsErr := sqliteShadowReadImports(filepath.Join(s.root, "imports.json"))
		if readJobsErr != nil || readImportsErr != nil {
			s.primaryFatal = fmt.Errorf("cannot validate SQL-first error recovery: jobs=%v, imports=%v", readJobsErr, readImportsErr)
			return s.primaryFatal
		}
		if verifyErr := s.primary.verifyJSONParity(context.Background(), onDiskJobs, onDiskImports); verifyErr != nil {
			s.primaryFatal = fmt.Errorf("SQL/JSON drift after failed SQL-first write: %w", verifyErr)
			return s.primaryFatal
		}
		if sqliteShadowEqual(jobs, onDiskJobs) && sqliteShadowEqual(imports, onDiskImports) {
			return nil // SQLite and JSON committed the new state despite a late I/O error.
		}
		return err // SQL did not commit; caller may restore its old memory image.
	}
}

package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// sqlitePrimaryReadSnapshotLocked reads the complete jobs/import inventory
// through ONE consistent SQL transaction and validates every indexed ACL and
// identity against the payload. It deliberately never substitutes JSON when
// SQLite is unavailable. Storage.mu must be held by the caller.
//
// This candidate read layer still compares the SQL generation to in-memory
// state because background workers and filesystem operations currently use
// the in-memory maps. A mismatch quarantines the candidate backend rather
// than permitting older in-memory ACLs to be served or rewritten over SQL.
// Production SQL-primary selection is not enabled by this read layer.
func (s *Storage) sqlitePrimaryReadSnapshotLocked() (map[string]Job, map[string]AuthorImport, error) {
	if s.primary == nil {
		return nil, nil, errors.New("SQL-primary reader is not configured")
	}
	if s.primaryFatal != nil {
		return nil, nil, s.primaryFatal
	}
	if s.shadow != nil {
		return nil, nil, s.sqlitePrimaryQuarantineLocked(errors.New("SQL-primary read conflicts with JSON shadow writer"))
	}
	ctx := context.Background()
	tx, err := s.primary.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, s.sqlitePrimaryQuarantineLocked(fmt.Errorf("cannot begin SQL-primary read: %w", err))
	}
	defer tx.Rollback()
	jobs, err := sqliteShadowSQLJobs(ctx, tx)
	if err != nil {
		return nil, nil, s.sqlitePrimaryQuarantineLocked(fmt.Errorf("invalid SQL-primary archive snapshot: %w", err))
	}
	imports, err := sqliteShadowSQLImports(ctx, tx)
	if err != nil {
		return nil, nil, s.sqlitePrimaryQuarantineLocked(fmt.Errorf("invalid SQL-primary import snapshot: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, s.sqlitePrimaryQuarantineLocked(fmt.Errorf("cannot commit SQL-primary read: %w", err))
	}
	if len(jobs) != len(s.jobs) || len(imports) != len(s.imports) {
		return nil, nil, s.sqlitePrimaryQuarantineLocked(errors.New("SQL-primary inventory differs from in-memory generation"))
	}
	for id, sqlJob := range jobs {
		current := s.jobs[id]
		if current == nil || !sqliteShadowEqual(*current, sqlJob) {
			return nil, nil, s.sqlitePrimaryQuarantineLocked(fmt.Errorf("SQL-primary archive generation differs: %s", id))
		}
	}
	for id, sqlImport := range imports {
		current := s.imports[id]
		if current == nil || !sqliteShadowEqual(*current, sqlImport) {
			return nil, nil, s.sqlitePrimaryQuarantineLocked(fmt.Errorf("SQL-primary author import generation differs: %s", id))
		}
	}
	return jobs, imports, nil
}

func (s *Storage) sqlitePrimaryQuarantineLocked(cause error) error {
	if s.primaryFatal == nil {
		s.primaryFatal = fmt.Errorf("SQL-primary storage quarantined: %w", cause)
	}
	return s.primaryFatal
}

// sqlitePrimaryGuardMutationLocked prevents metadata mutations, archive
// deletion and physical revision restores from acting on stale in-memory
// ownership or grants. Call BEFORE changing maps or touching archive files,
// with Storage.mu held. Legacy JSON mode has no extra SQL dependency.
//
// This uses the full checked SQL generation until production SQL-primary has
// an audited, constant-time generation fence; it is intentionally not a live
// SQL-primary activation switch.
func (s *Storage) sqlitePrimaryGuardMutationLocked() error {
	if s.primaryFatal != nil {
		return s.primaryFatal
	}
	if s.primary == nil {
		return nil
	}
	// Both mutation guards and verified projections need proof of parity,
	// not a second materialized inventory. Validate all SQL rows and ACLs
	// in one read transaction. Callers may project detached values from
	// the verified memory generation while still holding Storage.mu.
	if s.shadow != nil {
		return s.sqlitePrimaryQuarantineLocked(errors.New("SQL-primary read conflicts with JSON shadow writer"))
	}
	ctx := context.Background()
	tx, err := s.primary.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return s.sqlitePrimaryQuarantineLocked(err)
	}
	defer tx.Rollback()
	jobCount, importCount := 0, 0
	if err := sqliteShadowVisitJobs(ctx, tx, func(id string, job Job) error {
		current := s.jobs[id]
		if current == nil || !sqliteShadowEqual(*current, job) {
			return fmt.Errorf("SQL-primary archive generation differs: %s", id)
		}
		jobCount++
		return nil
	}); err != nil {
		return s.sqlitePrimaryQuarantineLocked(err)
	}
	if err := sqliteShadowVisitImports(ctx, tx, func(id string, imp AuthorImport) error {
		current := s.imports[id]
		if current == nil || !sqliteShadowEqual(*current, imp) {
			return fmt.Errorf("SQL-primary import generation differs: %s", id)
		}
		importCount++
		return nil
	}); err != nil {
		return s.sqlitePrimaryQuarantineLocked(err)
	}
	if jobCount != len(s.jobs) || importCount != len(s.imports) {
		return s.sqlitePrimaryQuarantineLocked(errors.New("SQL-primary inventory differs from in-memory generation"))
	}
	if err := tx.Commit(); err != nil {
		return s.sqlitePrimaryQuarantineLocked(err)
	}
	return nil
}

// guardArchiveFileMutation checks the internal SQL-primary snapshot and job
// identity immediately before archive file writes. A successful job claim
// alone is not enough: external drift could occur during a Boosty request.
// Call without Storage.mu; JSON-primary has no extra SQL read path.
func (s *Storage) guardArchiveFileMutation(job Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return err
	}
	if s.primary == nil {
		return nil
	}
	current := s.jobs[job.ID]
	if current == nil || current.OwnerID != job.OwnerID ||
		current.Author != job.Author || current.PostID != job.PostID ||
		!current.CreatedAt.Equal(job.CreatedAt) ||
		job.State != "running" || job.DeletedAt != nil ||
		current.State != "running" || current.DeletedAt != nil {
		return s.sqlitePrimaryQuarantineLocked(errors.New("archive generation changed before physical write"))
	}
	return nil
}

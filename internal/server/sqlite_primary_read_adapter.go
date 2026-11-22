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
	_, _, err := s.sqlitePrimaryReadSnapshotLocked()
	return err
}

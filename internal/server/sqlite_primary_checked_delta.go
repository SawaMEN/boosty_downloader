package server

import (
	"context"
	"database/sql"
	"fmt"
)

// applyCheckedPrimaryDelta verifies the entire before generation inside the
// SAME transaction that applies changes. A concurrent external SQL writer
// cannot slip between a separate read transaction and the write snapshot.
// The journal must be durable before any mutation or extra callback.
func (m *sqliteMetadata) applyCheckedPrimaryDelta(ctx context.Context,
	beforeJobs map[string]Job, beforeImports map[string]AuthorImport,
	jobs map[string]Job, imports map[string]AuthorImport,
	stage func() error, extra func(context.Context, *sql.Tx) error) error {
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := sqliteVerifyParityTx(ctx, tx, beforeJobs, beforeImports); err != nil {
		return fmt.Errorf("SQL before-image does not match existing JSON: %w", err)
	}
	if err := stage(); err != nil {
		return err
	}
	for id, want := range jobs {
		have, exists := beforeJobs[id]
		if !exists {
			if err := sqliteApplyArchive(ctx, tx, sqliteArchiveChange{After: want}); err != nil {
				return err
			}
		} else if !sqliteShadowEqual(have, want) {
			if err := sqliteApplyArchive(ctx, tx, sqliteArchiveChange{Before: &have, After: want}); err != nil {
				return err
			}
		}
	}
	for id, have := range beforeJobs {
		if _, exists := jobs[id]; exists {
			continue
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM owner_archives WHERE archive_id=? AND owner_id=?", id, have.OwnerID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("SQL archive deletion conflict: %s", id)
		}
	}
	for id, want := range imports {
		have, exists := beforeImports[id]
		if !exists {
			if err := sqliteApplyImport(ctx, tx, sqliteImportChange{After: want}); err != nil {
				return err
			}
		} else if !sqliteShadowEqual(have, want) {
			if err := sqliteApplyImport(ctx, tx, sqliteImportChange{Before: &have, After: want}); err != nil {
				return err
			}
		}
	}
	for id, have := range beforeImports {
		if _, exists := imports[id]; exists {
			continue
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM author_imports WHERE import_id=? AND owner_id=?", id, have.OwnerID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("SQL import deletion conflict: %s", id)
		}
	}
	if extra != nil {
		if err := extra(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

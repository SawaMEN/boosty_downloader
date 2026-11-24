package server

import (
	"context"
	"database/sql"
)

type sqliteShadowRemoval struct {
	id, owner string
}

type sqliteShadowDeltaPlan struct {
	archives       []sqliteArchiveChange
	removeArchives []sqliteShadowRemoval
	importChanges  []sqliteImportChange
	removeImports  []sqliteShadowRemoval
}

// Validate the complete existing SQL inventory before any writes. Keep only
// desired IDs and actual changes; unchanged payloads can be collected as
// soon as the cursor advances. No database mutation occurs in this function.
func sqlitePlanShadowDelta(ctx context.Context, tx *sql.Tx, jobs map[string]Job, imports map[string]AuthorImport) (sqliteShadowDeltaPlan, error) {
	var plan sqliteShadowDeltaPlan
	seenJobs := make(map[string]struct{}, len(jobs))
	if err := sqliteShadowVisitJobs(ctx, tx, func(id string, have Job) error {
		want, exists := jobs[id]
		if !exists {
			plan.removeArchives = append(plan.removeArchives, sqliteShadowRemoval{id: id, owner: have.OwnerID})
			return nil
		}
		seenJobs[id] = struct{}{}
		if !sqliteShadowEqual(have, want) {
			plan.archives = append(plan.archives, sqliteArchiveChange{Before: &have, After: want})
		}
		return nil
	}); err != nil {
		return sqliteShadowDeltaPlan{}, err
	}
	for id, want := range jobs {
		if _, exists := seenJobs[id]; !exists {
			plan.archives = append(plan.archives, sqliteArchiveChange{After: want})
		}
	}
	seenImports := make(map[string]struct{}, len(imports))
	if err := sqliteShadowVisitImports(ctx, tx, func(id string, have AuthorImport) error {
		want, exists := imports[id]
		if !exists {
			plan.removeImports = append(plan.removeImports, sqliteShadowRemoval{id: id, owner: have.OwnerID})
			return nil
		}
		seenImports[id] = struct{}{}
		if !sqliteShadowEqual(have, want) {
			plan.importChanges = append(plan.importChanges, sqliteImportChange{Before: &have, After: want})
		}
		return nil
	}); err != nil {
		return sqliteShadowDeltaPlan{}, err
	}
	for id, want := range imports {
		if _, exists := seenImports[id]; !exists {
			plan.importChanges = append(plan.importChanges, sqliteImportChange{After: want})
		}
	}
	return plan, nil
}

// The caller owns the transaction used to build the plan. Never apply it
// against a different SQL snapshot.
func (p sqliteShadowDeltaPlan) apply(ctx context.Context, tx *sql.Tx) error {
	for _, change := range p.archives {
		if err := sqliteApplyArchive(ctx, tx, change); err != nil {
			return err
		}
	}
	for _, old := range p.removeArchives {
		// Only SQL metadata, never filesystem archives or media.
		if _, err := tx.ExecContext(ctx, "DELETE FROM owner_archives WHERE archive_id=? AND owner_id=?", old.id, old.owner); err != nil {
			return err
		}
	}
	for _, change := range p.importChanges {
		if err := sqliteApplyImport(ctx, tx, change); err != nil {
			return err
		}
	}
	for _, old := range p.removeImports {
		if _, err := tx.ExecContext(ctx, "DELETE FROM author_imports WHERE import_id=? AND owner_id=?", old.id, old.owner); err != nil {
			return err
		}
	}
	return nil
}

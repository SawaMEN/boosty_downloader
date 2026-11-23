package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Update only the published current revision inside the SQL-primary metadata
// transaction. Historical rows must never be deleted or rewritten by this
// operation. Other archives can be staging concurrently; avoid a whole-tree
// index rebuild while their download is in progress.
func sqliteArchivePublishCurrentRevision(ctx context.Context, tx *sql.Tx, revision sqliteDiskRevision, jobs map[string]Job) error {
	job, ok := jobs[revision.archiveID]
	if !ok || !revision.current || revision.id != "current:"+job.ID ||
		revision.owner != job.OwnerID || revision.author != job.Author ||
		!revision.captured.Equal(job.ArchiveUpdatedAt) {
		return errors.New("SQL current revision does not match published archive metadata")
	}
	old, err := sqliteReindexOldRevisions(ctx, tx, jobs)
	if err != nil {
		return err
	}
	if previous, exists := old[revision.id]; exists {
		if previous.archiveID != job.ID || previous.path != revision.path {
			return errors.New("SQL current revision owner or path changed")
		}
	}
	var observation any
	if previous, exists := old[revision.id]; exists {
		assets, err := sqliteReindexOldAssets(ctx, tx)
		if err != nil { return err }
		same, err := sqliteReindexUnchanged(ctx, tx, previous, revision, assets[revision.id])
		if err != nil { return err }
		if same && previous.observation.Valid { observation = previous.observation.Int64 }
	}
	if observation == nil {
		observation, err = sqliteReindexObservation(ctx, tx, revision)
		if err != nil { return err }
	}
	if _, err := tx.ExecContext(ctx,"DELETE FROM archive_revisions WHERE revision_id=? AND archive_id=? AND is_current=1",revision.id,job.ID); err != nil {
		return err
	}
	if err := sqliteReindexWriteRevision(ctx, tx, revision, observation); err != nil {
		return fmt.Errorf("cannot index newly published archive: %w",err)
	}
	// Drop unused SQL blob records, but never unlink the physical CAS.
	_, err = tx.ExecContext(ctx,`DELETE FROM media_blobs WHERE NOT EXISTS
		(SELECT 1 FROM revision_assets a WHERE a.sha256=media_blobs.sha256)`)
	return err
}

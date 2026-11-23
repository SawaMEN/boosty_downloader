package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
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
		if err != nil {
			return err
		}
		same, err := sqliteReindexUnchanged(ctx, tx, previous, revision, assets[revision.id])
		if err != nil {
			return err
		}
		if same && previous.observation.Valid {
			observation = previous.observation.Int64
		}
	}
	if observation == nil {
		observation, err = sqliteReindexObservation(ctx, tx, revision)
		if err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM archive_revisions WHERE revision_id=? AND archive_id=? AND is_current=1", revision.id, job.ID); err != nil {
		return err
	}
	if err := sqliteReindexWriteRevision(ctx, tx, revision, observation); err != nil {
		return fmt.Errorf("cannot index newly published archive: %w", err)
	}
	// Drop unused SQL blob records, but never unlink the physical CAS.
	_, err = tx.ExecContext(ctx, `DELETE FROM media_blobs WHERE NOT EXISTS
		(SELECT 1 FROM revision_assets a WHERE a.sha256=media_blobs.sha256)`)
	return err
}

// Compare the immutable SQL revision projection with the actual manifest
// and media. Never use an unindexed or cross-owner snapshot for restore.
func sqliteVerifyIndexedRevision(ctx context.Context, tx *sql.Tx, revision sqliteDiskRevision, jobs map[string]Job) error {
	old, err := sqliteReindexOldRevisions(ctx, tx, jobs)
	if err != nil { return err }
	row, ok := old[revision.id]
	if !ok { return errors.New("archive revision is missing from SQL index") }
	assets, err := sqliteReindexOldAssets(ctx, tx)
	if err != nil { return err }
	equal, err := sqliteReindexUnchanged(ctx, tx, row, revision, assets[revision.id])
	if err != nil { return err }
	if !equal { return errors.New("archive revision differs from indexed SQL bytes or identity") }
	return nil
}

func (s *Storage) verifySQLRestoreRevisionLocked(job Job, id string, jobs map[string]Job) error {
	history, err := sqliteReindexReadRevision(s.root,job,id,
		[]string{"revisions",job.OwnerID,job.Author,job.PostID,id},false)
	if err != nil { return err }
	current, err := sqliteReindexReadRevision(s.root,job,"current:"+job.ID,
		[]string{"library",job.OwnerID,job.Author,job.PostID},true)
	if err != nil { return err }
	ctx:=context.Background()
	tx,err:=s.primary.db.BeginTx(ctx,&sql.TxOptions{ReadOnly:true})
	if err != nil { return err }
	defer tx.Rollback()
	for _,revision:=range []sqliteDiskRevision{history,current} {
		if err:=sqliteVerifyIndexedRevision(ctx,tx,revision,jobs);err!=nil{return err}
	}
	return tx.Commit()
}

// Resolve the SQL-authoritative commit point before a file journal can
// classify the JSON generation. Caller holds Storage.mu.
func (s *Storage) recoverSQLFileCommitLocked() error {
	if err := recoverMetadataPair(s.root); err != nil { return err }
	if err := recoverSQLitePrimaryCommitLocked(s.root); err != nil { return err }
	jobs, err := sqliteShadowReadJobs(filepath.Join(s.root,"jobs.json"))
	if err != nil { return err }
	imports, err := sqliteShadowReadImports(filepath.Join(s.root,"imports.json"))
	if err != nil { return err }
	return s.primary.verifyJSONParity(context.Background(),jobs,imports)
}

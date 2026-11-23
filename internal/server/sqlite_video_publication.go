package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
)

func sqliteVideoEventDetail(j videoPublicationJournal) string {
	raw, _ := json.Marshal(struct{ Nonce, Manifest string }{j.Nonce, j.AfterManifest})
	return string(raw)
}

func sqliteVideoMetadata(root string, j videoPublicationJournal, jobsRaw []byte) (map[string]Job, map[string]AuthorImport, error) {
	if metadataDigest(jobsRaw) != j.JobsHash {
		return nil, nil, errors.New("SQL video jobs generation changed")
	}
	importsRaw, err := videoPublicationReadFile(root, "imports.json", 64<<20)
	if err != nil || metadataDigest(importsRaw) != j.ImportsHash {
		return nil, nil, errors.New("SQL video imports generation changed")
	}
	return sqlitePrimaryDecodeImages(jobsRaw, importsRaw)
}

func sqliteVideoParity(ctx context.Context, tx *sql.Tx, jobs map[string]Job, imports map[string]AuthorImport) error {
	actualJobs, err := sqliteShadowSQLJobs(ctx, tx)
	if err != nil || !sqliteShadowEqual(jobs, actualJobs) {
		return errors.New("SQL video jobs or ACL diverged")
	}
	actualImports, err := sqliteShadowSQLImports(ctx, tx)
	if err != nil || !sqliteShadowEqual(imports, actualImports) {
		return errors.New("SQL video imports diverged")
	}
	return nil
}

func (s *Storage) verifySQLVideoCurrentLocked(job Job) error {
	jobs, err := sqliteShadowReadJobs(filepath.Join(s.root, "jobs.json"))
	if err != nil {
		return err
	}
	rev, err := sqliteReindexReadRevision(s.root, job, "current:"+job.ID, []string{"library", job.OwnerID, job.Author, job.PostID}, true)
	if err != nil {
		return err
	}
	ctx := context.Background()
	tx, err := s.primary.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := sqliteVerifyIndexedRevision(ctx, tx, rev, jobs); err != nil {
		return err
	}
	return tx.Commit()
}

// Unlike job commits, a rendition changes neither jobs nor imports. Use an
// event nonce committed atomically with the current revision index as the
// durable decision, not a fabricated job change or the filesystem phase.
func (s *Storage) commitSQLVideoLocked(j videoPublicationJournal) error {
	raw, err := videoPublicationReadFile(s.root, "jobs.json", 64<<20)
	if err != nil {
		return err
	}
	jobs, imports, err := sqliteVideoMetadata(s.root, j, raw)
	if err != nil {
		return err
	}
	job, ok := jobs[j.JobID]
	if !ok || job.DeletedAt != nil || job.OwnerID != j.Owner || job.Author != j.Author || job.PostID != j.Post {
		return errors.New("SQL video owner changed")
	}
	rev, err := sqliteReindexReadRevision(s.root, job, "current:"+job.ID, []string{"library", job.OwnerID, job.Author, job.PostID}, true)
	if err != nil {
		return err
	}
	ctx := context.Background()
	tx, err := s.primary.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := sqliteVideoParity(ctx, tx, jobs, imports); err != nil {
		return err
	}
	var count int
	detail := sqliteVideoEventDetail(j)
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM archive_events WHERE event_type='video_publication' AND detail_json=?", detail).Scan(&count); err != nil || count != 0 {
		return errors.New("duplicate SQL video publication event")
	}
	if err := sqliteArchivePublishCurrentRevision(ctx, tx, rev, jobs); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO archive_events(owner_id,archive_id,event_type,created_at,detail_json) VALUES(?,?,?,?,?)",
		job.OwnerID, job.ID, "video_publication", sqliteDate(job.UpdatedAt), detail); err != nil {
		return err
	}
	return tx.Commit()
}

// Project the chosen manifest from verified on-disk media and the pinned
// staged rendition, not from the possibly half-published live manifest.
func sqliteVideoExpectedRevision(root string, j videoPublicationJournal, job Job, man ArchiveManifest, stage string, commit bool) (sqliteDiskRevision, error) {
	rev := sqliteDiskRevision{id: "current:" + job.ID, archiveID: job.ID, owner: job.OwnerID, author: job.Author,
		path:  filepath.ToSlash(filepath.Join("library", job.OwnerID, job.Author, job.PostID)),
		title: man.Title, captured: man.CapturedAt, subscription: man.Subscription, current: true}
	if man.Author != job.Author || man.PostID != job.PostID || !man.CapturedAt.Equal(job.ArchiveUpdatedAt) || len(man.Assets) > 5000 {
		return rev, errors.New("SQL video manifest identity mismatch")
	}
	if sqliteReindexDigestValid(man.ContentSHA256) {
		rev.fingerprint = man.ContentSHA256
	}
	seen := map[string]bool{}
	for _, asset := range man.Assets {
		if !safeArchivePart(asset.Filename) || seen[asset.Filename] {
			return rev, errors.New("invalid SQL video media identity")
		}
		seen[asset.Filename] = true
		var path string
		var err error
		if asset.Filename == j.Target {
			if commit {
				path = filepath.Join(stage, "media.new")
			} else if j.HadOldMedia {
				path = filepath.Join(stage, "media.old")
			} else {
				return rev, errors.New("missing previous video media")
			}
		} else {
			path, err = sqliteAuditSafePath(root, []string{"library", job.OwnerID, job.Author, job.PostID, "media", asset.Filename})
			if err != nil {
				return rev, err
			}
		}
		digest, size, err := sqliteReindexHashFile(path)
		if err != nil {
			return rev, err
		}
		if (asset.Size != 0 && asset.Size != size) || (asset.SHA256 != "" && asset.SHA256 != digest) {
			return rev, fmt.Errorf("SQL video source bytes changed: %s", asset.Filename)
		}
		rev.assets = append(rev.assets, sqliteDiskAsset{path: filepath.ToSlash(filepath.Join("media", asset.Filename)), role: asset.Type, digest: digest, size: size})
	}
	return rev, nil
}

func sqliteVideoRecoveryDecision(root string, j videoPublicationJournal, job Job, jobsRaw []byte, oldMan, newMan ArchiveManifest, stage string) (bool, error) {
	jobs, imports, err := sqliteVideoMetadata(root, j, jobsRaw)
	if err != nil {
		return false, err
	}
	db, err := openSQLiteReadOnlyMode(filepath.Join(root, "metadata.db"), false)
	if err != nil {
		return false, err
	}
	defer db.Close()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if err := sqliteVideoParity(ctx, tx, jobs, imports); err != nil {
		return false, err
	}
	var total, owned int
	detail := sqliteVideoEventDetail(j)
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN owner_id=? AND archive_id=? THEN 1 ELSE 0 END),0)
		FROM archive_events WHERE event_type='video_publication' AND detail_json=?`, job.OwnerID, job.ID, detail).Scan(&total, &owned); err != nil {
		return false, err
	}
	if total > 1 || owned != total || (j.Phase == "committed" && total != 1) {
		return false, errors.New("SQL video event identity or commit decision differs")
	}
	commit := total == 1
	man := oldMan
	if commit {
		man = newMan
	}
	rev, err := sqliteVideoExpectedRevision(root, j, job, man, stage, commit)
	if err != nil {
		return false, err
	}
	if err := sqliteVerifyIndexedRevision(ctx, tx, rev, jobs); err != nil {
		return false, err
	}
	return commit, tx.Commit()
}

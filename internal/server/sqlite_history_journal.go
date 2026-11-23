package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const sqliteHistoryJournalName = ".sqlite-history-transaction.json"

// Adding immutable history is a roll-forward operation: after the intent is
// durable the exact pinned stage may be published and indexed repeatedly.
// It never changes the live archive, jobs, ACLs or prior historical rows.
type sqliteHistoryJournal struct {
	Version                                        int
	JobID, Owner, Author, Post, Revision           string
	JobsHash, ImportsHash, ManifestHash, FilesHash string
	Device, Inode                                  uint64
}

func sqliteHistoryPaths(root string, j sqliteHistoryJournal) (string, string, error) {
	if j.Version != 1 || !safeArchivePart(j.JobID) || !safeArchivePart(j.Owner) ||
		!safeArchivePart(j.Author) || !safeArchivePart(j.Post) || !validRevisionID(j.Revision) ||
		!sqliteReindexDigestValid(j.JobsHash) || !sqliteReindexDigestValid(j.ImportsHash) ||
		!sqliteReindexDigestValid(j.ManifestHash) || !sqliteReindexDigestValid(j.FilesHash) || j.Inode == 0 {
		return "", "", errors.New("invalid SQL history journal identity")
	}
	parent := filepath.Join(root, "revisions", j.Owner, j.Author, j.Post)
	return filepath.Join(parent, ".incomplete-"+j.Revision), filepath.Join(parent, j.Revision), nil
}

// Bind every allowlisted byte, not just the media referenced in SQL. The
// archived post, comments and HTML must survive a crash unchanged too.
func sqliteHistoryFilesHash(base string) (string, error) {
	files, _, err := archiveFiles(base)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	encoder := json.NewEncoder(h)
	for _, relative := range files {
		f, err := openArchiveRegular(base, filepath.ToSlash(relative))
		if os.IsNotExist(err) && relative != "index.html" && relative != "manifest.json" &&
			filepath.Dir(relative) != "media" {
			if err := encoder.Encode([]any{relative, false}); err != nil {
				return "", err
			}
			continue
		}
		if err != nil {
			return "", err
		}
		content := sha256.New()
		n, copyErr := io.Copy(content, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		if err := encoder.Encode([]any{relative, true, n, hex.EncodeToString(content.Sum(nil))}); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Storage) publishSQLHistoryLocked(job Job, id, stage string) error {
	for _, pending := range []string{sqliteHistoryJournalName, sqliteHistoryDeleteJournalName, sqlitePrimaryTransactionName, metadataJournalName,
		sqliteRollbackJournalName, sqliteCutoverIntentName, purgeJournalName, revisionRestoreJournalName, archiveUpdateJournalName, videoPublicationJournalName} {
		if _, err := os.Lstat(filepath.Join(s.root, pending)); err == nil {
			return errors.New("history publication blocked by pending transaction: " + pending)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	jobs, err := videoPublicationReadFile(s.root, "jobs.json", 64<<20)
	if err != nil {
		return err
	}
	imports, err := videoPublicationReadFile(s.root, "imports.json", 64<<20)
	if err != nil {
		return err
	}
	manifest, _, err := readArchiveManifestNoFollow(stage)
	if err != nil {
		return err
	}
	filesHash, err := sqliteHistoryFilesHash(stage)
	if err != nil {
		return err
	}
	dev, inode, err := revisionRestoreIdentity(s.root, stage)
	if err != nil {
		return err
	}
	j := sqliteHistoryJournal{Version: 1, JobID: job.ID, Owner: job.OwnerID, Author: job.Author, Post: job.PostID,
		Revision: id, JobsHash: metadataDigest(jobs), ImportsHash: metadataDigest(imports), ManifestHash: metadataDigest(manifest),
		FilesHash: filesHash, Device: dev, Inode: inode}
	expected, dest, err := sqliteHistoryPaths(s.root, j)
	if err != nil || expected != stage {
		return errors.New("unexpected SQL history staging path")
	}
	if exists, err := purgeSafePath(s.root, dest); err != nil || exists != nil {
		return errors.New("history destination occupied")
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := writeSyncedMetadata(s.root, sqliteHistoryJournalName, raw); err != nil {
		if _, readErr := os.Lstat(filepath.Join(s.root, sqliteHistoryJournalName)); !os.IsNotExist(readErr) {
			return s.sqlitePrimaryQuarantineLocked(err)
		}
		return err
	}
	if s.sqliteHistoryCheckpoint != nil {
		if err := s.sqliteHistoryCheckpoint("prepared"); err != nil {
			return s.sqlitePrimaryQuarantineLocked(err)
		}
	}
	if err := recoverSQLHistoryLocked(s.root, s.sqliteHistoryCheckpoint); err != nil {
		return s.sqlitePrimaryQuarantineLocked(err)
	}
	return nil
}

func recoverSQLHistoryLocked(root string, checkpoint func(string) error) error {
	info, err := os.Lstat(filepath.Join(root, sqliteHistoryJournalName))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > 16<<10 {
		return errors.New("unsafe SQL history journal")
	}
	raw, err := videoPublicationReadFile(root, sqliteHistoryJournalName, 16<<10)
	if err != nil {
		return err
	}
	var j sqliteHistoryJournal
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&j); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing SQL history journal data")
	}
	stage, dest, err := sqliteHistoryPaths(root, j)
	if err != nil {
		return err
	}
	for _, pending := range []string{sqliteHistoryDeleteJournalName, sqlitePrimaryTransactionName, metadataJournalName, sqliteRollbackJournalName,
		sqliteCutoverIntentName, purgeJournalName, revisionRestoreJournalName, archiveUpdateJournalName, videoPublicationJournalName} {
		if _, err := os.Lstat(filepath.Join(root, pending)); err == nil {
			return errors.New("conflicting history transaction: " + pending)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	jobsRaw, err := videoPublicationReadFile(root, "jobs.json", 64<<20)
	if err != nil || metadataDigest(jobsRaw) != j.JobsHash {
		return errors.New("SQL history jobs generation changed")
	}
	importsRaw, err := videoPublicationReadFile(root, "imports.json", 64<<20)
	if err != nil || metadataDigest(importsRaw) != j.ImportsHash {
		return errors.New("SQL history imports generation changed")
	}
	jobs, imports, err := sqlitePrimaryDecodeImages(jobsRaw, importsRaw)
	if err != nil {
		return err
	}
	job, ok := jobs[j.JobID]
	if !ok || job.OwnerID != j.Owner || job.Author != j.Author || job.PostID != j.Post || job.DeletedAt != nil {
		return errors.New("SQL history owner or job changed")
	}
	staged, err := revisionRestoreDirectory(root, stage, j.ManifestHash, j.Author, j.Post, j.Device, j.Inode, true)
	if err != nil {
		return err
	}
	published, err := revisionRestoreDirectory(root, dest, j.ManifestHash, j.Author, j.Post, j.Device, j.Inode, true)
	if err != nil {
		return err
	}
	if staged == published {
		return errors.New("SQL history stage and published directory are ambiguous")
	}
	source := dest
	if staged {
		source = stage
	}
	if digest, err := sqliteHistoryFilesHash(source); err != nil || digest != j.FilesHash {
		return errors.New("SQL history snapshot bytes changed")
	}
	parts := []string{"revisions", j.Owner, j.Author, j.Post, filepath.Base(source)}
	revision, err := sqliteReindexReadRevision(root, job, j.Revision, parts, false)
	if err != nil {
		return err
	}
	revision.path = filepath.ToSlash(filepath.Join("revisions", j.Owner, j.Author, j.Post, j.Revision))
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		return err
	}
	defer meta.close()
	ctx := context.Background()
	tx, err := meta.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	sqlJobs, err := sqliteShadowSQLJobs(ctx, tx)
	if err != nil || !sqliteShadowEqual(jobs, sqlJobs) {
		return errors.New("SQL history jobs or ACL diverged")
	}
	sqlImports, err := sqliteShadowSQLImports(ctx, tx)
	if err != nil || !sqliteShadowEqual(imports, sqlImports) {
		return errors.New("SQL history imports diverged")
	}
	old, err := sqliteReindexOldRevisions(ctx, tx, jobs)
	if err != nil {
		return err
	}
	if existing, ok := old[j.Revision]; ok {
		if staged {
			return errors.New("history index exists before directory publication")
		}
		assets, err := sqliteReindexOldAssets(ctx, tx)
		if err != nil {
			return err
		}
		same, err := sqliteReindexUnchanged(ctx, tx, existing, revision, assets[j.Revision])
		if err != nil || !same {
			return errors.New("published SQL historical revision differs")
		}
	} else {
		if staged {
			if err := durableRevisionRestoreRename(root, stage, dest); err != nil {
				return err
			}
		}
		if checkpoint != nil {
			if err := checkpoint("published"); err != nil {
				return err
			}
		}
		observation, err := sqliteReindexObservation(ctx, tx, revision)
		if err != nil {
			return err
		}
		if err := sqliteReindexWriteRevision(ctx, tx, revision, observation); err != nil {
			return fmt.Errorf("index historical revision: %w", err)
		}
	}
	// If an earlier rename survived but its fsync failed, make it durable
	// before committing the index. Never replace an existing history row.
	if err := syncMetadataDirectory(filepath.Dir(dest)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if checkpoint != nil {
		if err := checkpoint("indexed"); err != nil {
			return err
		}
	}
	return removeSyncedMetadata(root, sqliteHistoryJournalName)
}

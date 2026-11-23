package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// A restore is not a single filesystem operation: live -> backup,
// stage -> live, then jobs.json commit. The pinned JSON generation selects
// rollback or finalization after SIGKILL, before any archive is served.
// This journal intentionally supports JSON-authoritative storage only.
const revisionRestoreJournalName = ".archive-restore-transaction.json"

type revisionRestoreJournal struct {
	Version      int    `json:"version"`
	JobID        string `json:"jobId"`
	Owner        string `json:"owner"`
	Author       string `json:"author"`
	Post         string `json:"post"`
	Revision     string `json:"revision"`
	Nonce        string `json:"nonce"`
	BeforeHash   string `json:"beforeHash"`
	AfterHash    string `json:"afterHash"`
	OldManifest  string `json:"oldManifest"`
	NewManifest  string `json:"newManifest"`
}

func revisionRestorePaths(root string, journal revisionRestoreJournal) (string, string, string, error) {
	if !validRevisionID(journal.JobID) || !validRevisionID(journal.Revision) ||
		!validRevisionID(journal.Nonce) || !safeArchivePart(journal.Owner) ||
		!safeArchivePart(journal.Author) || !safeArchivePart(journal.Post) {
		return "", "", "", errors.New("invalid archive restore journal path")
	}
	parent := filepath.Join(root, "library", journal.Owner, journal.Author)
	return filepath.Join(parent, journal.Post),
		filepath.Join(parent, ".restore-"+journal.Nonce),
		filepath.Join(parent, ".previous-"+journal.Nonce), nil
}

func validateRevisionRestoreJournal(j revisionRestoreJournal) error {
	if j.Version != 1 || !purgeValidHex(j.BeforeHash) ||
		!purgeValidHex(j.AfterHash) || j.BeforeHash == j.AfterHash ||
		!purgeValidHex(j.OldManifest) || !purgeValidHex(j.NewManifest) {
		return errors.New("invalid archive restore generation or checksum")
	}
	return nil
}

// Check the identity before removing a directory as part of crash recovery.
// The manifest is read without following a symlink, as are all ancestors.
// A missing directory is distinct from an invalid directory.
func revisionRestoreDirectory(root, dir, expected, author, post string) (bool, error) {
	info, err := purgeSafePath(root, dir)
	if err != nil || info == nil {
		return false, err
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	file, err := os.Lstat(manifestPath)
	if err != nil {
		return true, err
	}
	if !file.Mode().IsRegular() {
		return true, errors.New("archive restore manifest is not a regular file")
	}
	bytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return true, err
	}
	if metadataDigest(bytes) != expected {
		return true, fmt.Errorf("archive restore directory manifest changed: %s", dir)
	}
	var man ArchiveManifest
	if err := json.Unmarshal(bytes, &man); err != nil || man.Author != author || man.PostID != post {
		return true, errors.New("archive restore directory belongs to a different post")
	}
	return true, nil
}

func durableRevisionRestoreRename(root, from, to string) error {
	if origin, err := purgeSafePath(root, from); err != nil || origin == nil {
		return errors.New("archive restore rename source is absent or unsafe")
	}
	if target, err := purgeSafePath(root, to); err != nil || target != nil {
		return errors.New("archive restore destination is occupied or unsafe")
	}
	if err := unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	return syncMetadataDirectory(filepath.Dir(from))
}

func durableRevisionRestoreRemove(root, dir string) error {
	if info, err := purgeSafePath(root, dir); err != nil {
		return err
	} else if info == nil {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return syncMetadataDirectory(filepath.Dir(dir))
}

func writeRevisionRestoreJournal(root string, j revisionRestoreJournal) error {
	if err := validateRevisionRestoreJournal(j); err != nil {
		return err
	}
	if _, _, _, err := revisionRestorePaths(root, j); err != nil {
		return err
	}
	path := filepath.Join(root, revisionRestoreJournalName)
	if _, err := os.Lstat(path); err == nil {
		return errors.New("archive restore transaction already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	return writeSyncedMetadata(root, revisionRestoreJournalName, data)
}

// recoverArchiveRestoreLocked runs under a data-dir process lock at startup,
// or under Storage.mu during in-process error handling. It never guesses the
// commit state from the presence of a folder: only jobs.json is authoritative.
func recoverArchiveRestoreLocked(root string) error {
	path := filepath.Join(root, revisionRestoreJournalName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > 16<<10 {
		return errors.New("unsafe archive restore transaction journal")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var j revisionRestoreJournal
	if err := decoder.Decode(&j); err != nil {
		return fmt.Errorf("damaged archive restore transaction: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("archive restore transaction has extra data")
	}
	if err := validateRevisionRestoreJournal(j); err != nil {
		return err
	}
	live, stage, backup, err := revisionRestorePaths(root, j)
	if err != nil {
		return err
	}
	jobsPath := filepath.Join(root, "jobs.json")
	jobsInfo, err := os.Lstat(jobsPath)
	if err != nil || !jobsInfo.Mode().IsRegular() {
		return errors.New("archive restore transaction has no safe jobs.json")
	}
	jobsRaw, err := os.ReadFile(jobsPath)
	if err != nil {
		return err
	}
	generation := metadataDigest(jobsRaw)
	before := generation == j.BeforeHash
	after := generation == j.AfterHash
	if !before && !after {
		return errors.New("archive restore found an unknown jobs.json generation")
	}
	var jobs map[string]Job
	if err := json.Unmarshal(jobsRaw, &jobs); err != nil {
		return err
	}
	current, exists := jobs[j.JobID]
	if !exists || current.OwnerID != j.Owner || current.Author != j.Author ||
		current.PostID != j.Post || current.DeletedAt != nil ||
		(current.State != "done" && current.State != "partial") {
		return errors.New("archive restore metadata does not match the owner and post")
	}
	// Validate EVERY existing path before mutating any of them. In particular,
	// reject symlinks and a replaced backup before deleting a live directory.
	liveExists, liveErr := revisionRestoreDirectory(root, live, j.OldManifest, j.Author, j.Post)
	stageExists, stageErr := revisionRestoreDirectory(root, stage, j.NewManifest, j.Author, j.Post)
	backupExists, backupErr := revisionRestoreDirectory(root, backup, j.OldManifest, j.Author, j.Post)
	if stageErr != nil || backupErr != nil {
		return fmt.Errorf("unsafe archive restore stage or backup: %w", errors.Join(stageErr, backupErr))
	}
	if liveExists {
		want := j.OldManifest
		if backupExists || after {
			want = j.NewManifest
		}
		liveExists, liveErr = revisionRestoreDirectory(root, live, want, j.Author, j.Post)
	}
	if liveErr != nil {
		return liveErr
	}
	if before {
		switch {
		case backupExists && liveExists && !stageExists:
			// The new version is active but metadata is old: discard it safely.
			if err := durableRevisionRestoreRemove(root, live); err != nil {
				return err
			}
			if err := durableRevisionRestoreRename(root, backup, live); err != nil {
				return err
			}
		case backupExists && !liveExists:
			// Crash after first rename or while rolling back the second.
			if err := durableRevisionRestoreRename(root, backup, live); err != nil {
				return err
			}
		case !backupExists && liveExists:
			// No swap occurred, or an interrupted rollback already finished.
		case !backupExists && !liveExists:
			return errors.New("uncommitted archive restore lost both original folders")
		default:
			return errors.New("unknown archive restore pre-commit folder layout")
		}
		if stageExists {
			if err := durableRevisionRestoreRemove(root, stage); err != nil {
				return err
			}
		}
	} else {
		// JSON commit is authoritative. The new live directory must exist;
		// never re-open old data or delete a backup when that is uncertain.
		if !liveExists || stageExists {
			return errors.New("committed archive restore has invalid folder layout")
		}
		if backupExists {
			if err := durableRevisionRestoreRemove(root, backup); err != nil {
				return err
			}
		}
	}
	finalJobs, err := os.ReadFile(jobsPath)
	if err != nil || metadataDigest(finalJobs) != generation {
		return errors.New("archive metadata changed while recovering restore")
	}
	finalJournal, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(finalJournal, raw) {
		return errors.New("archive restore journal changed during recovery")
	}
	return removeSyncedMetadata(root, revisionRestoreJournalName)
}

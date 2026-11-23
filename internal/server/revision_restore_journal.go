package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// A restore is not a single filesystem operation: live -> backup,
// stage -> live, then jobs.json commit. The pinned JSON generation selects
// rollback or finalization after SIGKILL, before any archive is served.
// This journal intentionally supports JSON-authoritative storage only.
const revisionRestoreJournalName = ".archive-restore-transaction.json"

type revisionRestoreJournal struct {
	Version     int    `json:"version"`
	JobID       string `json:"jobId"`
	Owner       string `json:"owner"`
	Author      string `json:"author"`
	Post        string `json:"post"`
	Revision    string `json:"revision"`
	Nonce       string `json:"nonce"`
	BeforeHash  string `json:"beforeHash"`
	AfterHash   string `json:"afterHash"`
	OldManifest string `json:"oldManifest"`
	NewManifest string `json:"newManifest"`
	OldDevice   uint64 `json:"oldDevice"`
	OldInode    uint64 `json:"oldInode"`
	NewDevice   uint64 `json:"newDevice"`
	NewInode    uint64 `json:"newInode"`
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
		!purgeValidHex(j.OldManifest) || !purgeValidHex(j.NewManifest) ||
		j.OldDevice == 0 || j.OldInode == 0 || j.NewDevice == 0 || j.NewInode == 0 {
		return errors.New("invalid archive restore generation or checksum")
	}
	return nil
}

// Rename preserves device/inode. Pin the original and the staged directory
// identities so that a partially removed stage/backup can be safely cleaned
// after another SIGKILL, even when its manifest was already unlinked.
// A substituted directory with identical files but a different inode fails.
func revisionRestoreIdentity(root, dir string) (uint64, uint64, error) {
	info, err := purgeSafePath(root, dir)
	if err != nil {
		return 0, 0, err
	}
	if info == nil {
		return 0, 0, errors.New("archive restore folder does not exist")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Dev == 0 || stat.Ino == 0 {
		return 0, 0, errors.New("archive restore cannot pin directory inode")
	}
	return uint64(stat.Dev), stat.Ino, nil
}

// requireManifest is false only for directories which will be deleted by
// this very recovery step. Original/live bytes being preserved must always
// have the expected manifest. Inode checks always apply, even during cleanup.
func revisionRestoreDirectory(root, dir, expected, author, post string, device, inode uint64, requireManifest bool) (bool, error) {
	info, err := purgeSafePath(root, dir)
	if err != nil || info == nil {
		return false, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Dev) != device || stat.Ino != inode {
		return true, errors.New("archive restore directory was replaced")
	}
	if !requireManifest {
		return true, nil
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	file, err := os.Lstat(manifestPath)
	if err != nil {
		return true, err
	}
	if !file.Mode().IsRegular() {
		return true, errors.New("archive restore manifest is not a regular file")
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return true, err
	}
	if metadataDigest(raw) != expected {
		return true, fmt.Errorf("archive restore directory manifest changed: %s", dir)
	}
	var man ArchiveManifest
	if err := json.Unmarshal(raw, &man); err != nil || man.Author != author || man.PostID != post {
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
	// Validate every current path before mutating any of them. Stage and
	// backup directories may already be half-removed by a previous recovery,
	// so their pinned device/inode is the safe deletion identity.
	stageExists, stageErr := revisionRestoreDirectory(root, stage, j.NewManifest, j.Author, j.Post,
		j.NewDevice, j.NewInode, false)
	backupExists, backupErr := revisionRestoreDirectory(root, backup, j.OldManifest, j.Author, j.Post,
		j.OldDevice, j.OldInode, before)
	if stageErr != nil || backupErr != nil {
		return fmt.Errorf("unsafe archive restore stage or backup: %w", errors.Join(stageErr, backupErr))
	}
	liveHash, liveDevice, liveInode := j.OldManifest, j.OldDevice, j.OldInode
	requireLiveManifest := true
	if backupExists || after {
		liveHash, liveDevice, liveInode = j.NewManifest, j.NewDevice, j.NewInode
		// Before metadata commits, the replacement live directory is about
		// to be discarded; it might already be partially deleted by a
		// previous interrupted rollback.
		if before {
			requireLiveManifest = false
		}
	}
	liveExists, liveErr := revisionRestoreDirectory(root, live, liveHash, j.Author, j.Post,
		liveDevice, liveInode, requireLiveManifest)
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

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const archiveUpdateJournalName = ".archive-update-transaction.json"
const archiveUpdateStagePrefix = ".update-stage-"
const archiveUpdateBackupPrefix = ".update-previous-"

type archiveUpdateJournal struct {
	Version     int    `json:"version"`
	Phase       string `json:"phase"`
	Nonce       string `json:"nonce"`
	JobID       string `json:"jobId"`
	Owner       string `json:"owner"`
	Author      string `json:"author"`
	Post        string `json:"post"`
	OldExists   bool   `json:"oldExists"`
	OldDevice   uint64 `json:"oldDevice"`
	OldInode    uint64 `json:"oldInode"`
	OldManifest string `json:"oldManifest"`
	NewDevice   uint64 `json:"newDevice"`
	NewInode    uint64 `json:"newInode"`
	NewManifest string `json:"newManifest"`
	BeforeJobs string `json:"beforeJobs,omitempty"`
	AfterJobs string `json:"afterJobs,omitempty"`
}

type archiveUpdateStage struct {
	Live  string
	Path  string
	Nonce string
}

func archiveUpdatePaths(root string, j archiveUpdateJournal) (live, stage, backup string, err error) {
	if (j.Version != 1 && j.Version != 2) || (j.Phase != "prepared" && j.Phase != "committed" && j.Phase != "aborting") ||
		(j.Version == 1 && (j.BeforeJobs != "" || j.AfterJobs != "")) ||
		(j.Version == 2 && (!purgeValidHex(j.BeforeJobs) || !purgeValidHex(j.AfterJobs) || j.BeforeJobs == j.AfterJobs)) ||
		!validRevisionID(j.Nonce) || !safeArchivePart(j.JobID) ||
		!safeArchivePart(j.Owner) || !safeArchivePart(j.Author) || !safeArchivePart(j.Post) ||
		!purgeValidHex(j.NewManifest) || j.NewDevice == 0 || j.NewInode == 0 ||
		(j.OldExists && (!purgeValidHex(j.OldManifest) || j.OldDevice == 0 || j.OldInode == 0)) ||
		(!j.OldExists && (j.OldManifest != "" || j.OldDevice != 0 || j.OldInode != 0)) {
		return "", "", "", errors.New("invalid archive update journal")
	}
	parent := filepath.Join(root, "library", j.Owner, j.Author)
	live = filepath.Join(parent, j.Post)
	stage = filepath.Join(parent, archiveUpdateStagePrefix+j.Nonce)
	backup = filepath.Join(parent, archiveUpdateBackupPrefix+j.Nonce)
	return
}

func archiveUpdateReadJournal(root string) (archiveUpdateJournal, bool, error) {
	p := filepath.Join(root, archiveUpdateJournalName)
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return archiveUpdateJournal{}, false, nil
	}
	if err != nil {
		return archiveUpdateJournal{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > 16<<10 {
		return archiveUpdateJournal{}, true, errors.New("unsafe archive update transaction journal")
	}
	file, err := openArchiveRegular(root, archiveUpdateJournalName)
	if err != nil {
		return archiveUpdateJournal{}, true, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16<<10+1))
	if err != nil || len(data) > 16<<10 {
		return archiveUpdateJournal{}, true, errors.New("archive update journal exceeds its size limit")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var j archiveUpdateJournal
	if err := dec.Decode(&j); err != nil {
		return j, true, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return j, true, errors.New("archive update journal contains trailing data")
	}
	if _, _, _, err := archiveUpdatePaths(root, j); err != nil {
		return j, true, err
	}
	return j, true, nil
}

// beginArchiveUpdate creates a same-filesystem private snapshot before any
// network download or metadata write. Existing media are hardlinks: changing
// the staged copy is always by atomic replacement, never in-place truncation.
func (s *Storage) beginArchiveUpdate(job Job) (archiveUpdateStage, error) {
	if !safeArchivePart(job.OwnerID) || !safeArchivePart(job.Author) || !safeArchivePart(job.PostID) {
		return archiveUpdateStage{}, errors.New("unsafe archive update identity")
	}
	live := s.archiveDir(job.OwnerID, job.Author, job.PostID)
	if err := os.MkdirAll(filepath.Dir(live), 0700); err != nil {
		return archiveUpdateStage{}, err
	}
	nonce, err := newRevisionID()
	if err != nil {
		return archiveUpdateStage{}, err
	}
	stage := archiveUpdateStage{Live: live, Path: filepath.Join(filepath.Dir(live), archiveUpdateStagePrefix+nonce), Nonce: nonce}
	info, err := purgeSafePath(s.root, live)
	if err != nil {
		return stage, err
	}
	if info != nil {
		if !info.IsDir() {
			return stage, errors.New("existing archive is not a directory")
		}
		if _, err := linkArchiveSnapshot(live, stage.Path); err != nil {
			_ = os.RemoveAll(stage.Path)
			return stage, fmt.Errorf("cannot stage a valid existing archive: %w", err)
		}
	} else if err := os.Mkdir(stage.Path, 0700); err != nil {
		return stage, err
	}
	return stage, nil
}

func archiveUpdateRemoveUnpublishedMedia(folder string, man ArchiveManifest) error {
	media := filepath.Join(folder, "media")
	entries, err := os.ReadDir(media)
	if err != nil {
		return err
	}
	keep := make(map[string]int64, len(man.Assets))
	for _, a := range man.Assets {
		if !safeArchivePart(a.Filename) || a.Size < 0 {
			return errors.New("unsafe file in new archive manifest")
		}
		keep[a.Filename] = a.Size
	}
	for _, entry := range entries {
		path := filepath.Join(media, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("non-regular media in staged archive")
		}
		if size, included := keep[entry.Name()]; included {
			if info.Size() != size {
				return fmt.Errorf("staged media has wrong size: %s", entry.Name())
			}
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	for name := range keep {
		if _, err := os.Lstat(filepath.Join(media, name)); err != nil {
			return fmt.Errorf("missing staged media %s: %w", name, err)
		}
	}
	return syncMetadataDirectory(media)
}

// sync the directory metadata of all already-synced file content before the
// journal can outlive the power loss. All new downloaded bytes were fsynced
// by downloadFile; writeAtomic fsyncs JSON and HTML before rename.
func archiveUpdateSyncStage(stage string) error {
	media := filepath.Join(stage, "media")
	if info, err := os.Lstat(media); err == nil {
		if !info.IsDir() {
			return errors.New("unsafe staged media directory")
		}
		if err := syncMetadataDirectory(media); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := syncMetadataDirectory(stage); err != nil {
		return err
	}
	return syncMetadataDirectory(filepath.Dir(stage))
}

// Caller holds Storage.mu: progress, ACL and unrelated job writes must not
// change the authoritative JSON generation during the commit window.
func (s *Storage) archiveUpdatePrepare(job Job, stage archiveUpdateStage, beforeJobs, afterJobs string) (archiveUpdateJournal, error) {
	if !safeArchivePart(job.ID) || !safeArchivePart(job.OwnerID) || !safeArchivePart(job.Author) || !safeArchivePart(job.PostID) {
		return archiveUpdateJournal{}, errors.New("unsafe archive update job identity")
	}
	if _, existing, err := archiveUpdateReadJournal(s.root); err != nil || existing {
		return archiveUpdateJournal{}, errors.New("another archive update journal needs recovery")
	}
	current := s.jobs[job.ID]
	if current == nil || current.OwnerID != job.OwnerID || current.Author != job.Author || current.PostID != job.PostID ||
		current.DeletedAt != nil || (current.State != "running" && current.State != "done" && current.State != "partial") {
		return archiveUpdateJournal{}, errors.New("archive update job owner or state changed")
	}
	newRaw, newMan, err := readArchiveManifestNoFollow(stage.Path)
	if err != nil || newMan.Author != job.Author || newMan.PostID != job.PostID {
		return archiveUpdateJournal{}, errors.New("staged archive has unsafe or foreign manifest")
	}
	if err := archiveUpdateRemoveUnpublishedMedia(stage.Path, newMan); err != nil {
		return archiveUpdateJournal{}, err
	}
	if err := archiveUpdateSyncStage(stage.Path); err != nil {
		return archiveUpdateJournal{}, err
	}
	newDevice, newInode, err := revisionRestoreIdentity(s.root, stage.Path)
	if err != nil {
		return archiveUpdateJournal{}, err
	}
	j := archiveUpdateJournal{
		Version: 2, Phase: "prepared", Nonce: stage.Nonce,
		JobID: job.ID, Owner: job.OwnerID, Author: job.Author, Post: job.PostID,
		BeforeJobs: beforeJobs, AfterJobs: afterJobs,
		NewDevice: newDevice, NewInode: newInode, NewManifest: metadataDigest(newRaw),
	}
	old, err := purgeSafePath(s.root, stage.Live)
	if err != nil {
		return j, err
	}
	if old != nil {
		if !old.IsDir() {
			return j, errors.New("live archive is not a directory")
		}
		oldRaw, oldMan, err := readArchiveManifestNoFollow(stage.Live)
		if err != nil || oldMan.Author != job.Author || oldMan.PostID != job.PostID {
			return j, errors.New("live archive changed before publication")
		}
		j.OldExists = true
		j.OldManifest = metadataDigest(oldRaw)
		j.OldDevice, j.OldInode, err = revisionRestoreIdentity(s.root, stage.Live)
		if err != nil {
			return j, err
		}
	}
	if _, _, _, err := archiveUpdatePaths(s.root, j); err != nil {
		return j, err
	}
	data, err := json.Marshal(j)
	if err != nil {
		return j, err
	}
	if err := writeSyncedMetadata(s.root, archiveUpdateJournalName, data); err != nil {
		return j, err
	}
	return j, nil
}

// Determine the pinned identity of a directory without following a symlink.
func archiveUpdateWhich(root, path string, j archiveUpdateJournal, needManifest bool) (string, error) {
	info, err := purgeSafePath(root, path)
	if err != nil || info == nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("unsafe archive update directory")
	}
	dev, ino, err := revisionRestoreIdentity(root, path)
	if err != nil {
		return "", err
	}
	if dev == j.NewDevice && ino == j.NewInode {
		_, err := revisionRestoreDirectory(root, path, j.NewManifest, j.Author, j.Post,
			j.NewDevice, j.NewInode, needManifest)
		return "new", err
	}
	if j.OldExists && dev == j.OldDevice && ino == j.OldInode {
		_, err := revisionRestoreDirectory(root, path, j.OldManifest, j.Author, j.Post,
			j.OldDevice, j.OldInode, needManifest)
		return "old", err
	}
	return "", errors.New("archive update directory inode was replaced")
}

// recoverArchiveUpdateJournal runs before jobs/ACL are loaded on startup,
// or under the archive update mutex after an in-process publication error.
// It is idempotent through interruptions during cleanup or replay.
func recoverArchiveUpdateJournal(root string) (bool, error) {
	j, found, err := archiveUpdateReadJournal(root)
	if err != nil || !found {
		return false, err
	}
	live, stage, backup, err := archiveUpdatePaths(root, j)
	if err != nil {
		return false, err
	}
	jobsFile, err := videoPublicationReadFile(root, "jobs.json", 64<<20)
	if err != nil {
		return false, err
	}
	var jobs map[string]Job
	if err := json.Unmarshal(jobsFile, &jobs); err != nil {
		return false, err
	}
	job, exists := jobs[j.JobID]
	if !exists || job.OwnerID != j.Owner || job.Author != j.Author || job.PostID != j.Post || job.DeletedAt != nil {
		return false, errors.New("archive update job no longer belongs to the journal owner")
	}
	prepared := j.Phase != "committed"
	if j.Version == 2 {
		switch metadataDigest(jobsFile) {
		case j.BeforeJobs:
			if j.Phase == "committed" {
				return false, errors.New("committed archive update has old jobs metadata")
			}
			prepared = true
		case j.AfterJobs:
			if j.Phase == "aborting" {
				return false, errors.New("aborted archive update has new jobs metadata")
			}
			prepared = false
		default:
			return false, errors.New("archive update found unknown jobs.json generation")
		}
	}
	liveKind, err := archiveUpdateWhich(root, live, j, true)
	// A rollback may be interrupted while deleting the new directory.
	// Verify its inode first; its manifest may already be missing.
	if err != nil && prepared && (j.Version == 1 || j.Phase == "aborting") {
		if kind, second := archiveUpdateWhich(root, live, j, false); second == nil && kind == "new" {
			liveKind, err = kind, nil
		}
	}
	if err != nil {
		return false, err
	}
	// A prepared stage must still have the exact SHA-256 manifest that was
	// committed into the journal. Only an explicitly durable aborting phase
	// permits partially removed stage files on repeated-crash cleanup.
	stageKind, err := archiveUpdateWhich(root, stage, j, j.Phase == "prepared")
	if err != nil {
		return false, err
	}
	backupKind, err := archiveUpdateWhich(root, backup, j, prepared)
	if err != nil {
		return false, err
	}
	if stageKind != "" && stageKind != "new" || backupKind != "" && backupKind != "old" {
		return false, errors.New("archive update stage or backup identity changed")
	}
	if prepared {
		if j.Phase != "aborting" {
			j.Phase = "aborting"
			data, err := json.Marshal(j)
			if err != nil {
				return false, err
			}
			if err := writeSyncedMetadata(root, archiveUpdateJournalName, data); err != nil {
				return false, err
			}
		}
		if j.OldExists {
			switch {
			case backupKind == "old" && (liveKind == "new" || liveKind == ""):
				if liveKind == "new" {
					if err := durableRevisionRestoreRemove(root, live); err != nil {
						return false, err
					}
				}
				if err := durableRevisionRestoreRename(root, backup, live); err != nil {
					return false, err
				}
			case backupKind == "" && liveKind == "old":
				// No swap happened, or interrupted rollback finished.
			default:
				return false, errors.New("archive update lost original files during rollback")
			}
		} else {
			if backupKind != "" || liveKind == "old" {
				return false, errors.New("new archive rollback found a foreign backup")
			}
			if liveKind == "new" {
				if err := durableRevisionRestoreRemove(root, live); err != nil {
					return false, err
				}
			}
		}

		if stageKind == "new" {
			if err := durableRevisionRestoreRemove(root, stage); err != nil {
				return false, err
			}
		}
	} else {
		if liveKind != "new" || stageKind != "" {
			return false, errors.New("committed archive update has no verified new version")
		}
		if backupKind == "old" {
			if err := durableRevisionRestoreRemove(root, backup); err != nil {
				return false, err
			}
		}
	}
	if err := removeSyncedMetadata(root, archiveUpdateJournalName); err != nil {
		return !prepared, err
	}
	return !prepared, nil
}

// archiveUpdateFinalJob updates only archive-owned metadata. ACL grants,
// autosync and user options come from the current job, never a stale worker.
func archiveUpdateFinalJob(current, worker Job, man ArchiveManifest) Job {
	current.Title = man.Title
	current.Subscription = man.Subscription
	current.RequiredTier = man.RequiredTier
	current.ArchiveUpdatedAt = man.CapturedAt
	current.DoneFiles = 0
	for _, asset := range man.Assets {
		if asset.Type != "transcoded_video" {
			current.DoneFiles++
		}
	}
	current.TotalFiles = worker.TotalFiles
	if current.TotalFiles < current.DoneFiles {
		current.TotalFiles = current.DoneFiles
	}
	current.CommentsPartial = len(man.Failures) > 0 || man.CommentsStatus == "частично"
	current.State = "done"
	current.Error = ""
	if current.CommentsPartial {
		current.State = "partial"
		if len(man.Failures) > 0 {
			current.Error = fmt.Sprintf("архив частичный: %d ошибок; подробности в manifest.json", len(man.Failures))
		}
	}
	current.UpdatedAt = time.Now().UTC()
	return current
}

// Serialize brief commit windows across all job writers. Network transfers
// remain independent, and publishing one archive cannot overwrite another
// archive's data-root journal.
func (s *Storage) publishArchiveUpdate(job Job, stage archiveUpdateStage) (bool, error) {
	s.archiveUpdateMu.Lock()
	defer s.archiveUpdateMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return false, err
	}
	if s.primary != nil {
		return false, errors.New("whole-archive publication requires JSON-authoritative storage until SQL cutover")
	}
	checkpoint := func(phase string) error {
		if s.archiveUpdateCheckpoint != nil {
			return s.archiveUpdateCheckpoint(phase)
		}
		return nil
	}
	if err := recoverMetadataPair(s.root); err != nil {
		return false, err
	}
	if _, pending, err := archiveUpdateReadJournal(s.root); err != nil || pending {
		return false, errors.New("archive update requires journal recovery before new publication")
	}
	before, err := videoPublicationReadFile(s.root, "jobs.json", 64<<20)
	if err != nil {
		return false, err
	}
	var diskJobs map[string]Job
	if err := json.Unmarshal(before, &diskJobs); err != nil || !sqliteShadowEqual(s.jobs, diskJobs) {
		return false, errors.New("archive publication jobs.json does not match in-memory generation")
	}
	current := s.jobs[job.ID]
	if current == nil || current.OwnerID != job.OwnerID || current.Author != job.Author ||
		current.PostID != job.PostID || current.DeletedAt != nil {
		return false, errors.New("archive publication job identity changed")
	}
	_, man, err := readArchiveManifestNoFollow(stage.Path)
	if err != nil {
		return false, err
	}
	next := archiveUpdateFinalJob(*current, job, man)
	diskJobs[job.ID] = next
	after, err := json.MarshalIndent(diskJobs, "", "  ")
	if err != nil || len(after) > 64<<20 {
		return false, errors.New("archive publication cannot encode bounded jobs metadata")
	}
	beforeHash, afterHash := metadataDigest(before), metadataDigest(after)
	reconcile := func(cause error) (bool, error) {
		if _, err := recoverArchiveUpdateJournal(s.root); err != nil {
			s.primaryFatal = fmt.Errorf("archive update recovery failed: %w (initial error: %v)", err, cause)
			return false, s.primaryFatal
		}
		actual, err := videoPublicationReadFile(s.root, "jobs.json", 64<<20)
		if err != nil {
			s.primaryFatal = fmt.Errorf("archive update metadata cannot be classified: %w", err)
			return false, s.primaryFatal
		}
		switch metadataDigest(actual) {
		case afterHash:
			s.jobs[job.ID] = &next
			s.notifySQLiteShadowLocked()
			s.notifySQLiteMedia()
			return true, nil
		case beforeHash:
			if cause == nil {
				cause = errors.New("archive publication was rolled back")
			}
			return false, cause
		default:
			s.primaryFatal = errors.New("archive publication ended with unknown jobs metadata")
			return false, s.primaryFatal
		}
	}
	j, err := s.archiveUpdatePrepare(job, stage, beforeHash, afterHash)
	if err != nil {
		if _, found, readErr := archiveUpdateReadJournal(s.root); found || readErr != nil {
			return reconcile(err)
		}
		return false, err
	}
	if err := checkpoint("journal"); err != nil {
		return reconcile(err)
	}
	live, stagePath, backup, err := archiveUpdatePaths(s.root, j)
	if err != nil {
		return reconcile(err)
	}
	if j.OldExists {
		if err := durableRevisionRestoreRename(s.root, live, backup); err != nil {
			return reconcile(err)
		}
	}
	if err := checkpoint("old-moved"); err != nil {
		return reconcile(err)
	}
	if err := durableRevisionRestoreRename(s.root, stagePath, live); err != nil {
		return reconcile(err)
	}
	if err := checkpoint("new-moved"); err != nil {
		return reconcile(err)
	}
	// The exact jobs.json generation is the commit point. A crash or fsync
	// error after rename is classified by the bytes on disk, not intent.
	if err := writeSyncedMetadata(s.root, "jobs.json", after); err != nil {
		return reconcile(err)
	}
	if err := checkpoint("metadata"); err != nil {
		return reconcile(err)
	}
	j.Phase = "committed"
	journalBytes, err := json.Marshal(j)
	if err != nil {
		return reconcile(err)
	}
	if err := writeSyncedMetadata(s.root, archiveUpdateJournalName, journalBytes); err != nil {
		return reconcile(err)
	}
	if err := checkpoint("committed"); err != nil {
		return reconcile(err)
	}
	return reconcile(nil)
}

func archiveUpdateStageName(name string) bool {
	if !strings.HasPrefix(name, archiveUpdateStagePrefix) {
		return false
	}
	return validRevisionID(strings.TrimPrefix(name, archiveUpdateStagePrefix))
}

// Remove a pre-journal orphan created by SIGKILL while staging, without
// traversing links or deleting arbitrary user-supplied directories.
func cleanupAbandonedArchiveUpdates(root string) error {
	if _, pending, err := archiveUpdateReadJournal(root); err != nil {
		return err
	} else if pending {
		return errors.New("archive update journal still needs recovery")
	}
	library := filepath.Join(root, "library")
	owners, err := os.ReadDir(library)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, owner := range owners {
		if !safeArchivePart(owner.Name()) || !owner.IsDir() {
			return errors.New("unsafe archive update owner directory")
		}
		authors, err := os.ReadDir(filepath.Join(library, owner.Name()))
		if err != nil {
			return err
		}
		for _, author := range authors {
			if !safeArchivePart(author.Name()) || !author.IsDir() {
				return errors.New("unsafe archive update author directory")
			}
			parent := filepath.Join(library, owner.Name(), author.Name())
			entries, err := os.ReadDir(parent)
			if err != nil {
				return err
			}
			for _, entry := range entries {
				if !strings.HasPrefix(entry.Name(), archiveUpdateStagePrefix) {
					continue
				}
				if !archiveUpdateStageName(entry.Name()) || !entry.IsDir() {
					return errors.New("unsafe orphaned archive update staging directory")
				}
				path := filepath.Join(parent, entry.Name())
				if info, err := purgeSafePath(root, path); err != nil || info == nil {
					return errors.New("unsafe archive update staging path")
				}
				if err := os.RemoveAll(path); err != nil {
					return err
				}
				if err := syncMetadataDirectory(parent); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

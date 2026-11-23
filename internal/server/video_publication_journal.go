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
)

const videoPublicationJournalName = ".video-publication-transaction.json"
const videoPublicationStagePrefix = ".video-publish-"
const maxVideoPublicationHTML = 64 << 20

// A journal is durable before the first live media rename. Its phase is the
// commit decision. Prepared => restore old media + metadata. Committed =>
// finish new media + metadata. The four JSON/HTML images and both media
// hardlinks remain in a private staging directory until recovery finishes.
type videoPublicationJournal struct {
	Version         int
	Phase           string
	Nonce           string
	JobID           string
	Owner           string
	Author          string
	Post            string
	Target          string
	StageDevice     uint64
	StageInode      uint64
	BeforeManifest  string
	AfterManifest   string
	BeforeHTML      string
	AfterHTML       string
	HadOldMedia     bool
	BeforeMediaSHA  string
	BeforeMediaSize int64
	AfterMediaSHA   string
	AfterMediaSize  int64
	JobsHash string `json:",omitempty"`
	ImportsHash string `json:",omitempty"`
}

// All filesystem paths are derived from validated identifiers, never from a
// journal supplied absolute path. A journal cannot target another owner.
func videoPublicationPaths(root string, j videoPublicationJournal) (folder, stage, target string, err error) {
	if (j.Version != 1 && j.Version != 2) ||
		(j.Version == 2 && (!sqliteReindexDigestValid(j.JobsHash) || !sqliteReindexDigestValid(j.ImportsHash))) ||
		(j.Version == 1 && (j.JobsHash != "" || j.ImportsHash != "")) ||
		(j.Phase != "prepared" && j.Phase != "committed") ||
		!validRevisionID(j.Nonce) || !safeArchivePart(j.JobID) ||
		!safeArchivePart(j.Owner) || !safeArchivePart(j.Author) ||
		!safeArchivePart(j.Post) || !safeArchivePart(j.Target) ||
		j.StageDevice == 0 || j.StageInode == 0 ||
		!purgeValidHex(j.BeforeManifest) || !purgeValidHex(j.AfterManifest) ||
		!purgeValidHex(j.BeforeHTML) || !purgeValidHex(j.AfterHTML) ||
		!purgeValidHex(j.AfterMediaSHA) || j.AfterMediaSize <= 0 ||
		j.AfterMediaSize > maxArchiveMediaSize ||
		(j.HadOldMedia && (!purgeValidHex(j.BeforeMediaSHA) || j.BeforeMediaSize < 0)) ||
		(!j.HadOldMedia && (j.BeforeMediaSHA != "" || j.BeforeMediaSize != 0)) {
		return "", "", "", errors.New("invalid video publication journal")
	}
	folder = filepath.Join(root, "library", j.Owner, j.Author, j.Post)
	stage = filepath.Join(root, videoPublicationStagePrefix+j.Nonce)
	target = filepath.Join(folder, "media", j.Target)
	return
}

func videoPublicationReadFile(base, name string, limit int64) ([]byte, error) {
	file, err := openArchiveRegular(base, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() > limit {
		return nil, errors.New("video transaction file exceeds its safe limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, errors.New("video transaction file could not be read safely")
	}
	return data, nil
}

func videoPublicationReadJournal(root string) (videoPublicationJournal, bool, error) {
	path := filepath.Join(root, videoPublicationJournalName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return videoPublicationJournal{}, false, nil
	}
	if err != nil {
		return videoPublicationJournal{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 8192 {
		return videoPublicationJournal{}, true, errors.New("unsafe video publication journal")
	}
	raw, err := videoPublicationReadFile(root, videoPublicationJournalName, 8192)
	if err != nil {
		return videoPublicationJournal{}, true, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var j videoPublicationJournal
	if err := dec.Decode(&j); err != nil {
		return j, true, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return j, true, errors.New("video journal contains trailing data")
	}
	if _, _, _, err := videoPublicationPaths(root, j); err != nil {
		return j, true, err
	}
	return j, true, nil
}

func videoPublicationReadImage(stage, name, expected string, max int64) ([]byte, error) {
	data, err := videoPublicationReadFile(stage, name, max)
	if err != nil {
		return nil, err
	}
	if metadataDigest(data) != expected {
		return nil, fmt.Errorf("damaged video publication stage image %s", name)
	}
	return data, nil
}

func videoPublicationCheckMedia(path string, digest string, size int64) error {
	valid, err := verifyAsset(path, ArchiveAsset{SHA256: digest, Size: size})
	if err != nil || !valid {
		return fmt.Errorf("video publication staged media differs from its digest: %s", filepath.Base(path))
	}
	return nil
}

func videoPublicationMatchInode(path string, oldInfo, newInfo os.FileInfo, oldAllowed bool) error {
	current, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if oldAllowed {
			return errors.New("previous video rendition disappeared during publication")
		}
		return nil
	}
	if err != nil || !current.Mode().IsRegular() {
		return errors.New("unsafe video publication live media")
	}
	if os.SameFile(current, newInfo) || oldAllowed && os.SameFile(current, oldInfo) {
		return nil
	}
	return errors.New("video publication live inode was replaced")
}

// Replaying via a hardlink to a fixed staged inode is idempotent. A crash
// between temporary link creation and rename can be resumed after checking
// that the temporary link is exactly the expected staged file.
func videoPublicationReplayMedia(mediaDir, target, stagePath, nonce string, targetInfo os.FileInfo) error {
	dest := filepath.Join(mediaDir, target)
	source := filepath.Join(stagePath)
	temp := filepath.Join(mediaDir, ".video-replay-"+nonce)
	if info, err := os.Lstat(temp); err == nil {
		if !info.Mode().IsRegular() || !os.SameFile(info, targetInfo) {
			return errors.New("unexpected video replay temporary file")
		}
		if err := os.Remove(temp); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if current, err := os.Lstat(dest); err == nil && os.SameFile(current, targetInfo) {
		return syncMetadataDirectory(mediaDir)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Link(source, temp); err != nil {
		return err
	}
	if err := os.Rename(temp, dest); err != nil {
		return err
	}
	return syncMetadataDirectory(mediaDir)
}

// Must run under the data-dir process lock at startup, or under the writer
// gate and the publication mutex when handling an in-process write error.
// Never guess which image is safe: both images and all live targets are
// verified before the first replay mutation.
func recoverVideoPublicationJournal(root string) (bool, error) {
	j, found, err := videoPublicationReadJournal(root)
	if err != nil || !found {
		return false, err
	}
	folder, stage, dest, err := videoPublicationPaths(root, j)
	if err != nil {
		return false, err
	}
	dev, ino, err := revisionRestoreIdentity(root, stage)
	if err != nil || dev != j.StageDevice || ino != j.StageInode {
		return false, errors.New("video publication stage directory was replaced or lost")
	}
	oldManifest, err := videoPublicationReadImage(stage, "manifest.before", j.BeforeManifest, maxArchiveManifestBytes)
	if err != nil {
		return false, err
	}
	newManifest, err := videoPublicationReadImage(stage, "manifest.after", j.AfterManifest, maxArchiveManifestBytes)
	if err != nil {
		return false, err
	}
	oldHTML, err := videoPublicationReadImage(stage, "html.before", j.BeforeHTML, maxVideoPublicationHTML)
	if err != nil {
		return false, err
	}
	newHTML, err := videoPublicationReadImage(stage, "html.after", j.AfterHTML, maxVideoPublicationHTML)
	if err != nil {
		return false, err
	}
	var oldMan, newMan ArchiveManifest
	if json.Unmarshal(oldManifest, &oldMan) != nil || json.Unmarshal(newManifest, &newMan) != nil ||
		oldMan.Author != j.Author || oldMan.PostID != j.Post ||
		newMan.Author != j.Author || newMan.PostID != j.Post {
		return false, errors.New("video publication manifest belongs to another post")
	}
	// The metadata entry cannot be redirected to a different owner/post.
	jobsRaw, err := videoPublicationReadFile(root, "jobs.json", 64<<20)
	if err != nil {
		return false, err
	}
	var jobs map[string]Job
	if json.Unmarshal(jobsRaw, &jobs) != nil {
		return false, errors.New("video publication jobs metadata is invalid")
	}
	job, ok := jobs[j.JobID]
	if !ok || job.OwnerID != j.Owner || job.Author != j.Author || job.PostID != j.Post ||
		job.DeletedAt != nil {
		return false, errors.New("video publication owner or archive changed")
	}
	stageNew := filepath.Join(stage, "media.new")
	if err := videoPublicationCheckMedia(stageNew, j.AfterMediaSHA, j.AfterMediaSize); err != nil {
		return false, err
	}
	newInfo, err := os.Lstat(stageNew)
	if err != nil {
		return false, err
	}
	var oldInfo os.FileInfo
	if j.HadOldMedia {
		if err := videoPublicationCheckMedia(filepath.Join(stage, "media.old"), j.BeforeMediaSHA, j.BeforeMediaSize); err != nil {
			return false, err
		}
		oldInfo, err = os.Lstat(filepath.Join(stage, "media.old"))
		if err != nil {
			return false, err
		}
	}
	if err := videoPublicationMatchInode(dest, oldInfo, newInfo, j.HadOldMedia); err != nil {
		return false, err
	}
	currentManifest, _, err := readArchiveManifestNoFollow(folder)
	if err != nil {
		return false, err
	}
	currentHTML, err := videoPublicationReadFile(folder, "index.html", maxVideoPublicationHTML)
	if err != nil {
		return false, err
	}
	if d := metadataDigest(currentManifest); d != j.BeforeManifest && d != j.AfterManifest {
		return false, errors.New("video publication live manifest has an unknown generation")
	}
	if d := metadataDigest(currentHTML); d != j.BeforeHTML && d != j.AfterHTML {
		return false, errors.New("video publication HTML has an unknown generation")
	}
	commit := j.Phase == "committed"
	if j.Version == 2 {
		// SQL's event and revision index commit atomically. The filesystem
		// phase can lag behind an already committed SQL transaction.
		commit, err = sqliteVideoRecoveryDecision(root,j,job,jobsRaw,oldMan,newMan,stage)
		if err != nil { return false, err }
	}
	mediaDir := filepath.Join(folder, "media")
	// A crash between link and rename may leave the OTHER valid image
	// linked at the replay temp name. Remove only the pinned old/new inode,
	// including when the destination already matches or did not exist.
	temp := filepath.Join(mediaDir, ".video-replay-"+j.Nonce)
	if info,err:=os.Lstat(temp);err==nil {
		if !info.Mode().IsRegular() || (!os.SameFile(info,newInfo) && !(j.HadOldMedia && os.SameFile(info,oldInfo))) {
			return false, errors.New("unexpected video replay temporary inode")
		}
		if err:=os.Remove(temp);err!=nil{return false,err}
		if err:=syncMetadataDirectory(mediaDir);err!=nil{return false,err}
	} else if !os.IsNotExist(err) {return false,err}
	if commit {
		if err := videoPublicationReplayMedia(mediaDir, j.Target, stageNew, j.Nonce, newInfo); err != nil {
			return false, err
		}
		if err := writeSyncedMetadata(folder, "manifest.json", newManifest); err != nil {
			return false, err
		}
		if err := writeSyncedMetadata(folder, "index.html", newHTML); err != nil {
			return false, err
		}
	} else {
		if j.HadOldMedia {
			if err := videoPublicationReplayMedia(mediaDir, j.Target, filepath.Join(stage, "media.old"), j.Nonce, oldInfo); err != nil {
				return false, err
			}
		} else if oldMedia, err := os.Lstat(dest); err == nil {
			if !os.SameFile(oldMedia, newInfo) {
				return false, errors.New("video rollback refused unknown media")
			}
			if err := os.Remove(dest); err != nil {
				return false, err
			}
			if err := syncMetadataDirectory(mediaDir); err != nil {
				return false, err
			}
		} else if !os.IsNotExist(err) {
			return false, err
		}
		if err := writeSyncedMetadata(folder, "manifest.json", oldManifest); err != nil {
			return false, err
		}
		if err := writeSyncedMetadata(folder, "index.html", oldHTML); err != nil {
			return false, err
		}
	}
	// Finalize the journal first. An interrupted cleanup leaves an orphaned
	// stage that startup may safely reclaim without replaying the transaction.
	final,found,err:=videoPublicationReadJournal(root)
	if err!=nil||!found||final!=j{return false,errors.New("video journal changed during recovery")}
	if err := removeSyncedMetadata(root, videoPublicationJournalName); err != nil {
		return false, err
	}
	if err := cleanupAbandonedVideoPublicationStages(root); err != nil {
		return commit, err
	}
	return commit, nil
}

func videoPublicationPrepare(root string, job Job, target, staged string, manifestBefore, manifestAfter, htmlAfter []byte, mediaSize int64, mediaDigest string) (videoPublicationJournal, error) {
	return videoPublicationPrepareMode(root,job,target,staged,manifestBefore,manifestAfter,htmlAfter,mediaSize,mediaDigest,false)
}

func videoPublicationPrepareMode(root string, job Job, target, staged string, manifestBefore, manifestAfter, htmlAfter []byte, mediaSize int64, mediaDigest string, sqlPrimary bool) (videoPublicationJournal, error) {
	if !safeArchivePart(job.ID) || !safeArchivePart(job.OwnerID) ||
		!safeArchivePart(job.Author) || !safeArchivePart(job.PostID) || !safeArchivePart(target) ||
		!purgeValidHex(mediaDigest) || mediaSize <= 0 || mediaSize > maxArchiveMediaSize ||
		len(manifestBefore) > maxArchiveManifestBytes || len(manifestAfter) > maxArchiveManifestBytes ||
		len(htmlAfter) > maxVideoPublicationHTML {
		return videoPublicationJournal{}, errors.New("invalid video publication stage parameters")
	}
	if _, existing, err := videoPublicationReadJournal(root); err != nil || existing {
		return videoPublicationJournal{}, errors.New("another video publication is pending; restart for recovery")
	}
	folder := filepath.Join(root, "library", job.OwnerID, job.Author, job.PostID)
	htmlBefore, err := videoPublicationReadFile(folder, "index.html", maxVideoPublicationHTML)
	if err != nil {
		return videoPublicationJournal{}, err
	}
	nonce, err := newRevisionID()
	if err != nil {
		return videoPublicationJournal{}, err
	}
	j := videoPublicationJournal{
		Version: 1, Phase: "prepared", Nonce: nonce, JobID: job.ID,
		Owner: job.OwnerID, Author: job.Author, Post: job.PostID, Target: target,
		BeforeManifest: metadataDigest(manifestBefore), AfterManifest: metadataDigest(manifestAfter),
		BeforeHTML: metadataDigest(htmlBefore), AfterHTML: metadataDigest(htmlAfter),
		AfterMediaSHA: mediaDigest, AfterMediaSize: mediaSize,
	}
	if sqlPrimary {
		j.Version=2
		jobs,err:=videoPublicationReadFile(root,"jobs.json",64<<20)
		if err!=nil{return j,err}
		imports,err:=videoPublicationReadFile(root,"imports.json",64<<20)
		if err!=nil{return j,err}
		j.JobsHash,j.ImportsHash=metadataDigest(jobs),metadataDigest(imports)
	}
	_, stage, dest, err := videoPublicationPathsForStage(root, j)
	if err != nil {
		return j, err
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		return j, err
	}
	prepared := false
	defer func() {
		if !prepared {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := syncMetadataDirectory(root); err != nil {
		return j, err
	}
	for name, data := range map[string][]byte{
		"manifest.before": manifestBefore, "manifest.after": manifestAfter,
		"html.before": htmlBefore, "html.after": htmlAfter,
	} {
		if err := writeAtomic(filepath.Join(stage, name), data); err != nil {
			return j, err
		}
	}
	stagedInfo, err := os.Lstat(staged)
	if err != nil || !stagedInfo.Mode().IsRegular() {
		return j, errors.New("unsafe staged video file")
	}
	if err := os.Link(staged, filepath.Join(stage, "media.new")); err != nil {
		return j, err
	}
	if err := videoPublicationCheckMedia(filepath.Join(stage, "media.new"), mediaDigest, mediaSize); err != nil {
		return j, err
	}
	if old, err := os.Lstat(dest); err == nil {
		if !old.Mode().IsRegular() {
			return j, errors.New("unsafe existing video rendition")
		}
		j.HadOldMedia = true
		j.BeforeMediaSize, j.BeforeMediaSHA, err = fileDigest(dest)
		if err != nil {
			return j, err
		}
		if err := os.Link(dest, filepath.Join(stage, "media.old")); err != nil {
			return j, err
		}
		if err := videoPublicationCheckMedia(filepath.Join(stage, "media.old"), j.BeforeMediaSHA, j.BeforeMediaSize); err != nil {
			return j, err
		}
	} else if !os.IsNotExist(err) {
		return j, err
	}
	// An encoder may not have synced its output. Ensure the staged inode
	// survives a power failure before journal publication can reference it.
	file, err := os.Open(filepath.Join(stage, "media.new"))
	if err != nil {
		return j, err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if syncErr != nil {
		return j, syncErr
	}
	if closeErr != nil {
		return j, closeErr
	}
	if err := syncMetadataDirectory(stage); err != nil {
		return j, err
	}
	j.StageDevice, j.StageInode, err = revisionRestoreIdentity(root, stage)
	if err != nil {
		return j, err
	}
	data, err := json.Marshal(j)
	if err != nil {
		return j, err
	}
	if err := writeSyncedMetadata(root, videoPublicationJournalName, data); err != nil {
		// The atomic rename may have published the journal before fsync failed.
		if _, exists, _ := videoPublicationReadJournal(root); exists {
			prepared = true
		}
		return j, err
	}
	prepared = true
	return j, nil
}

// Validate the staged path using the fixed, checked identifiers before the
// media inode and manifest hashes are available.
func videoPublicationPathsForStage(root string, j videoPublicationJournal) (string, string, string, error) {
	if !validRevisionID(j.Nonce) || !safeArchivePart(j.JobID) || !safeArchivePart(j.Owner) ||
		!safeArchivePart(j.Author) || !safeArchivePart(j.Post) || !safeArchivePart(j.Target) {
		return "", "", "", errors.New("unsafe video publication staging path")
	}
	folder := filepath.Join(root, "library", j.Owner, j.Author, j.Post)
	return folder, filepath.Join(root, videoPublicationStagePrefix+j.Nonce), filepath.Join(folder, "media", j.Target), nil
}

func publishVideoRenditionDurably(root string, j videoPublicationJournal, checkpoint func(string) error) (bool, error) {
	return publishVideoRenditionDurablyWithSQL(root,j,checkpoint,nil)
}

func publishVideoRenditionDurablyWithSQL(root string, j videoPublicationJournal, checkpoint func(string) error, commitSQL func() error) (bool, error) {
	abort := func(cause error) (bool, error) {
		committed, err := recoverVideoPublicationJournal(root)
		if err != nil {
			return false, fmt.Errorf("video publication recovery required: %w (original error: %v)", err, cause)
		}
		if committed {
			return true, nil
		}
		return false, cause
	}
	fail := func(name string) error {
		if checkpoint != nil {
			return checkpoint(name)
		}
		return nil
	}
	if err := fail("journal"); err != nil {
		return abort(err)
	}
	folder, stage, dest, err := videoPublicationPaths(root, j)
	if err != nil {
		return abort(err)
	}
	newInfo, err := os.Lstat(filepath.Join(stage, "media.new"))
	if err != nil || !newInfo.Mode().IsRegular() {
		return abort(errors.New("video publication stage media is missing or unsafe"))
	}
	if err := videoPublicationReplayMedia(filepath.Dir(dest), j.Target, filepath.Join(stage, "media.new"), j.Nonce, newInfo); err != nil {
		return abort(err)
	}
	if err := fail("media"); err != nil {
		return abort(err)
	}
	manifest, err := videoPublicationReadImage(stage, "manifest.after", j.AfterManifest, maxArchiveManifestBytes)
	if err != nil {
		return abort(err)
	}
	if err := writeSyncedMetadata(folder, "manifest.json", manifest); err != nil {
		return abort(err)
	}
	if err := fail("manifest"); err != nil {
		return abort(err)
	}
	html, err := videoPublicationReadImage(stage, "html.after", j.AfterHTML, maxVideoPublicationHTML)
	if err != nil {
		return abort(err)
	}
	if err := writeSyncedMetadata(folder, "index.html", html); err != nil {
		return abort(err)
	}
	if err := fail("html"); err != nil {
		return abort(err)
	}
	if j.Version==2 {
		if commitSQL==nil{return abort(errors.New("missing SQL video commit callback"))}
		if err:=fail("sql-prepared");err!=nil{return abort(err)}
		if err:=commitSQL();err!=nil{return abort(err)}
		if err:=fail("sql-committed");err!=nil{return abort(err)}
	} else if commitSQL!=nil{return abort(errors.New("unexpected SQL video commit callback"))}
	j.Phase = "committed"
	data, err := json.Marshal(j)
	if err != nil {
		return abort(err)
	}
	if err := writeSyncedMetadata(root, videoPublicationJournalName, data); err != nil {
		return abort(err)
	}
	if err := fail("committed"); err != nil {
		return abort(err)
	}
	committed, err := recoverVideoPublicationJournal(root)
	if err != nil {
		return false, err
	}
	if !committed {
		return false, errors.New("video publication unexpectedly rolled back")
	}
	return true, nil
}

func videoPublicationStageNameValid(name string) bool {
	if !strings.HasPrefix(name, videoPublicationStagePrefix) {
		return false
	}
	return validRevisionID(strings.TrimPrefix(name, videoPublicationStagePrefix))
}

// A crash before the journal exists may leave a private stage directory.
// Reclaim only the fixed, untrusted-object-free shape created above.
func cleanupAbandonedVideoPublicationStages(root string) error {
	if _, found, err := videoPublicationReadJournal(root); err != nil {
		return err
	} else if found {
		return errors.New("cannot clean video publication staging with a pending journal")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, videoPublicationStagePrefix) {
			continue
		}
		if !videoPublicationStageNameValid(name) || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("unsafe orphaned video publication staging directory")
		}
		path := filepath.Join(root, name)
		if info, err := purgeSafePath(root, path); err != nil || info == nil {
			return errors.New("unsafe orphaned video publication staging path")
		}
		children, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		for _, child := range children {
			switch child.Name() {
			case "manifest.before", "manifest.after", "html.before", "html.after", "media.old", "media.new":
			default:
				if !strings.HasPrefix(child.Name(), ".write-") {
					return errors.New("unexpected video publication staging object")
				}
			}
			if !child.Type().IsRegular() {
				return errors.New("unsafe video publication staging child")
			}
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return syncMetadataDirectory(root)
}

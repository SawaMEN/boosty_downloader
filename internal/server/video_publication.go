package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// publishVideoRendition is the short critical section after an expensive
// FFmpeg run. Encoding itself never mutates live media or its manifest.
// Callers remove the staged file if publication returns false or an error.
func (a *App) publishVideoRendition(job Job, capturedAt time.Time, source ArchiveAsset, targetName, staged string, size int64, digest string) (bool, error) {
	if source.Filename == targetName || !safeArchivePart(source.Filename) || !safeArchivePart(targetName) ||
		source.Type != "ok_video" || source.SHA256 == "" || source.Size <= 0 {
		return false, errors.New("unsafe video source or rendition name")
	}
	unlock, ready := a.archiveGates.tryWrite(job)
	if !ready {
		return false, nil
	}
	defer unlock()

	// Lock order: archive gate, video journal mutex, metadata mutex.
	// A concurrent trash/ACL change must not race with a durable journal.
	a.store.videoPublicationMu.Lock()
	defer a.store.videoPublicationMu.Unlock()
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if err := a.store.sqlitePrimaryGuardMutationLocked(); err != nil {
		return false, err
	}
	entry := a.store.jobs[job.ID]
	if entry == nil || entry.DeletedAt != nil {
		return false, nil
	}
	current := *entry
	if (current.State != "done" && current.State != "partial") ||
		current.OwnerID != job.OwnerID || current.Author != job.Author ||
		current.PostID != job.PostID || !current.ArchiveUpdatedAt.Equal(job.ArchiveUpdatedAt) {
		return false, nil
	}
	if a.store.primary != nil {
		if err := sqliteHistoryDeleteGuard(a.store.root, false); err != nil {
			return false, err
		}
		if err := a.store.verifySQLVideoCurrentLocked(current); err != nil {
			return false, err
		}
	}
	folder := a.store.archiveDir(job.OwnerID, job.Author, job.PostID)
	originalJSON, latest, err := readArchiveManifestNoFollow(folder)
	if err != nil {
		return false, err
	}
	if latest.Author != job.Author || latest.PostID != job.PostID ||
		!latest.CapturedAt.Equal(capturedAt) {
		return false, nil
	}
	stillCurrent := false
	for _, asset := range latest.Assets {
		if asset.Filename == source.Filename && asset.Type == source.Type &&
			asset.SHA256 == source.SHA256 && asset.Size == source.Size {
			stillCurrent = true
		}
		if asset.Filename == targetName && (asset.Type != "transcoded_video" || asset.SourceFile != source.Filename) {
			return false, errors.New("video rendition would overwrite unrelated media")
		}
		if asset.Filename == targetName && asset.Type == "transcoded_video" &&
			asset.SourceSHA256 == source.SHA256 && asset.SHA256 != "" {
			dest := filepath.Join(folder, "media", targetName)
			if valid, _ := verifyAsset(dest, asset); valid {
				return true, nil // Another worker already published this rendition.
			}
		}
	}
	if !stillCurrent {
		return false, nil
	}
	if valid, err := verifyAsset(filepath.Join(folder, "media", source.Filename), source); err != nil || !valid {
		return false, nil // A modified source must never receive the old SHA-256.
	}
	if valid, err := verifyAsset(staged, ArchiveAsset{SHA256: digest, Size: size}); err != nil || !valid {
		return false, errors.New("staged rendition failed SHA-256 verification")
	}
	filtered := make([]ArchiveAsset, 0, len(latest.Assets)+1)
	for _, asset := range latest.Assets {
		// Other codec variants have independent media paths. Do not remove
		// their manifest/index references while leaving bytes on disk.
		if asset.Filename == targetName {
			continue
		}
		filtered = append(filtered, asset)
	}
	filtered = append(filtered, ArchiveAsset{
		Filename: targetName, Type: "transcoded_video", Title: source.Title,
		Size: size, SHA256: digest, SourceFile: source.Filename, SourceSHA256: source.SHA256,
	})
	latest.Assets = filtered
	latest.ContentSHA256 = ""
	// Build the HTML before changing anything, so a malformed old post does
	// not leave a new rendition with outdated generated presentation.
	html, err := renderVideoArchiveHTML(folder, latest)
	if err != nil {
		return false, err
	}
	manifestAfter, err := json.MarshalIndent(latest, "", "  ")
	if err != nil {
		return false, err
	}
	// Only one publication may own the data-root journal at a time. Archive
	// writer gate is already held, preventing refresh/restore of this post.
	journal, err := videoPublicationPrepareMode(a.store.root, current, targetName, staged, originalJSON, manifestAfter, html, size, digest, a.store.primary != nil)
	if err != nil {
		// A failed directory fsync may still have published the journal.
		if _, found, readErr := videoPublicationReadJournal(a.store.root); found && readErr == nil {
			if _, recoverErr := recoverVideoPublicationJournal(a.store.root); recoverErr != nil {
				a.store.primaryFatal = fmt.Errorf("video journal recovery failed: %w", recoverErr)
				return false, recoverErr
			}
		}
		return false, err
	}
	var commitSQL func() error
	if a.store.primary != nil {
		commitSQL = func() error { return a.store.commitSQLVideoLocked(journal) }
	}
	published, err := publishVideoRenditionDurablyWithSQL(a.store.root, journal, a.store.videoCheckpoint, commitSQL)
	if err != nil {
		// Fail closed if the recovery journal was not fully reconciled.
		if _, pending, _ := videoPublicationReadJournal(a.store.root); pending {
			a.store.primaryFatal = fmt.Errorf("video publication recovery is pending: %w", err)
		}
		return false, err
	}
	if !published {
		return false, nil
	}
	// The journal preserves the new inode until its durable commit and
	// cleanup. After success the caller's temporary link is no longer needed.
	_ = os.Remove(staged)
	a.store.notifySQLiteMedia()
	return true, nil
}

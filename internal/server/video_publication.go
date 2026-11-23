package server

import (
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
	if !safeArchivePart(source.Filename) || !safeArchivePart(targetName) ||
		source.Type != "ok_video" || source.SHA256 == "" || source.Size <= 0 {
		return false, errors.New("unsafe video source or rendition name")
	}
	unlock, ready := a.archiveGates.tryWrite(job)
	if !ready {
		return false, nil
	}
	defer unlock()

	current, exists := a.store.get(job.ID)
	if !exists || (current.State != "done" && current.State != "partial") ||
		current.OwnerID != job.OwnerID || current.Author != job.Author ||
		current.PostID != job.PostID || !current.ArchiveUpdatedAt.Equal(job.ArchiveUpdatedAt) {
		return false, nil
	}
	if err := a.store.guardArchiveFileMutation(current); err != nil {
		return false, fmt.Errorf("unsafe video publication generation: %w", err)
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
		if asset.Type == "transcoded_video" && asset.SourceFile == source.Filename {
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
	mediaDir := filepath.Join(folder, "media")
	dest := filepath.Join(mediaDir, targetName)
	// Preserve an older rendition until both metadata files are written.
	// This rollback covers ordinary errors; a future durable journal is still
	// needed for SIGKILL/power loss between multiple renames.
	var backup string
	if old, err := os.Lstat(dest); err == nil {
		if !old.Mode().IsRegular() {
			return false, errors.New("unsafe existing video rendition")
		}
		nonce, err := newRevisionID()
		if err != nil {
			return false, err
		}
		backup = filepath.Join(mediaDir, ".video-backup-"+nonce)
		if err := os.Link(dest, backup); err != nil {
			return false, err
		}
		defer os.Remove(backup)
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.Rename(staged, dest); err != nil {
		return false, err
	}
	rollback := func(cause error) (bool, error) {
		if err := writeAtomic(filepath.Join(folder, "manifest.json"), originalJSON); err != nil {
			a.store.mu.Lock()
			fatal := fmt.Errorf("video manifest rollback failed: %w", err)
			a.store.primaryFatal = fatal
			a.store.mu.Unlock()
			return false, fatal
		}
		var fileErr error
		if backup != "" {
			fileErr = os.Rename(backup, dest)
		} else {
			fileErr = os.Remove(dest)
		}
		if fileErr != nil {
			a.store.mu.Lock()
			fatal := fmt.Errorf("video file rollback failed: %w", fileErr)
			a.store.primaryFatal = fatal
			a.store.mu.Unlock()
			return false, fatal
		}
		return false, cause
	}
	if err := writeJSON(filepath.Join(folder, "manifest.json"), latest); err != nil {
		return rollback(err)
	}
	if err := writeAtomic(filepath.Join(folder, "index.html"), html); err != nil {
		return rollback(err)
	}
	a.store.notifySQLiteMedia()
	return true, nil
}

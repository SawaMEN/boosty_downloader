package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const fullSnapshotManifestName = "snapshot-manifest.json"
const fullSnapshotDataDirName = "data"
const fullSnapshotManifestMaxBytes = 64 << 20

// fullDataSnapshotManifest describes one offline copy of the entire data dir.
// The snapshot deliberately excludes host configuration under /etc, service
// binaries and TLS material stored outside the data directory.
type fullDataSnapshotManifest struct {
	Version    int                     `json:"version"`
	CapturedAt time.Time               `json:"capturedAt"`
	RootMode   uint32                  `json:"rootMode"`
	Entries    []fullDataSnapshotEntry `json:"entries"`
}

type fullDataSnapshotEntry struct {
	Path   string `json:"path"`
	Type   string `json:"type"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// PrepareFullDataSnapshot creates a new private, checksummed offline copy of
// the complete application data tree, including media, accounts and tokens.
// It never modifies source files. Restore is deliberately a separate future
// operation: users must not copy a snapshot over a running application.
func PrepareFullDataSnapshot(root, output string) (err error) {
	if root == "" || output == "" {
		return errors.New("data directory and new full snapshot directory are required")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	lock, err := lockDataDirectory(root)
	if err != nil {
		return err
	}
	defer lock.Close()

	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	dest, err := filepath.Abs(output)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(dest))
	if err != nil {
		return err
	}
	physicalDest := filepath.Join(parent, filepath.Base(dest))
	rel, err := filepath.Rel(physicalRoot, physicalDest)
	if err != nil {
		return err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return errors.New("full snapshot directory must be outside the live data directory")
	}
	if err := sqliteReindexNoWAL(root); err != nil {
		return fmt.Errorf("cannot snapshot unfinished JSON/SQLite transaction: %w", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "metadata.db-journal")); err == nil {
		return errors.New("cannot snapshot SQLite with a pending rollback journal")
	} else if !os.IsNotExist(err) {
		return err
	}

	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() {
		return errors.New("full snapshot source must be an existing directory")
	}
	if err := os.Mkdir(dest, 0700); err != nil {
		return err
	}
	created, err := os.Lstat(dest)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		// Never remove a destination which an external actor replaced.
		if !success {
			if now, e := os.Lstat(dest); e == nil && os.SameFile(created, now) {
				_ = os.RemoveAll(dest)
			}
		}
	}()
	dataDir := filepath.Join(dest, fullSnapshotDataDirName)
	if err := os.Mkdir(dataDir, 0700); err != nil {
		return err
	}

	manifest := fullDataSnapshotManifest{Version: 1, CapturedAt: time.Now().UTC(), RootMode: uint32(rootInfo.Mode().Perm())}
	directories := []string{dataDir}
	err = filepath.WalkDir(root, func(sourcePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if sourcePath == root {
			return nil
		}
		relative, err := filepath.Rel(root, sourcePath)
		if err != nil {
			return err
		}
		if !filepath.IsLocal(relative) {
			return fmt.Errorf("unsafe source path: %q", relative)
		}
		info, err := os.Lstat(sourcePath)
		if err != nil {
			return err
		}
		relativeSlash := filepath.ToSlash(relative)
		target := filepath.Join(dataDir, relative)
		item := fullDataSnapshotEntry{Path: relativeSlash, Mode: uint32(info.Mode().Perm())}
		if info.IsDir() {
			item.Type = "dir"
			if err := os.Mkdir(target, 0700); err != nil {
				return err
			}
			directories = append(directories, target)
		} else if info.Mode().IsRegular() {
			item.Type = "file"
			size, hash, err := fullSnapshotCopyFile(sourcePath, target, info)
			if err != nil {
				return err
			}
			item.Size, item.SHA256 = size, hash
		} else {
			return fmt.Errorf("unsafe symlink or special file in data directory: %s", relativeSlash)
		}
		manifest.Entries = append(manifest.Entries, item)
		return nil
	})
	if err != nil {
		return err
	}
	// Detect externally added, removed and replaced paths even if a writer
	// bypassed the application's directory lock while copying.
	if err := fullSnapshotCheckSourceTree(root, manifest.Entries); err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := syncMetadataDirectory(directories[i]); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if len(raw) > fullSnapshotManifestMaxBytes {
		return errors.New("full snapshot manifest exceeds 64 MiB")
	}
	if err := writeSyncedMetadata(dest, fullSnapshotManifestName, raw); err != nil {
		return err
	}
	if err := VerifyFullDataSnapshot(dest); err != nil {
		return fmt.Errorf("new full snapshot failed independent verification: %w", err)
	}
	if err := syncMetadataDirectory(dest); err != nil {
		return err
	}
	if err := syncMetadataDirectory(parent); err != nil {
		return err
	}
	success = true
	return nil
}

func fullSnapshotCopyFile(sourcePath, target string, original os.FileInfo) (int64, string, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return 0, "", err
	}
	defer source.Close()
	opened, err := source.Stat()
	if err != nil || !os.SameFile(original, opened) {
		return 0, "", fmt.Errorf("source changed while opening: %s", sourcePath)
	}
	dest, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 0, "", err
	}
	hasher := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(dest, hasher), source)
	if copyErr == nil {
		copyErr = dest.Sync()
	}
	if closeErr := dest.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return 0, "", copyErr
	}
	after, err := os.Lstat(sourcePath)
	if err != nil || !os.SameFile(opened, after) || opened.Size() != size ||
		after.Size() != size || !opened.ModTime().Equal(after.ModTime()) {
		return 0, "", fmt.Errorf("source changed while copying: %s", sourcePath)
	}
	// A second SHA-256 pass catches in-place edits during streaming that
	// preserve size and may have a timestamp intentionally reset by a writer.
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return 0, "", err
	}
	verifyHash := sha256.New()
	verifySize, err := io.Copy(verifyHash, source)
	if err != nil {
		return 0, "", err
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	if verifySize != size || hex.EncodeToString(verifyHash.Sum(nil)) != digest {
		return 0, "", fmt.Errorf("source bytes changed during snapshot: %s", sourcePath)
	}
	return size, digest, nil
}

func fullSnapshotCheckSourceTree(root string, entries []fullDataSnapshotEntry) error {
	found := make(map[string]fullDataSnapshotEntry, len(entries))
	for _, item := range entries {
		found[item.Path] = item
	}
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		expected, exists := found[key]
		if !exists {
			return fmt.Errorf("data directory changed while making snapshot: %s", key)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if uint32(info.Mode().Perm()) != expected.Mode ||
			(expected.Type == "file" && (!info.Mode().IsRegular() || info.Size() != expected.Size)) ||
			(expected.Type == "dir" && !info.IsDir()) {
			return fmt.Errorf("data directory entry changed during snapshot: %s", key)
		}
		count++
		return nil
	})
	if err != nil {
		return err
	}
	if count != len(entries) {
		return errors.New("files disappeared while creating full snapshot")
	}
	return nil
}

// VerifyFullDataSnapshot is independent of the live server and source paths.
// It checks every byte, privacy modes, paths and exact file inventory. A
// checksum manifest detects accidental changes, not malicious modification
// by a party that can rewrite both the manifest and all data.
func VerifyFullDataSnapshot(snapshot string) error {
	if snapshot == "" {
		return errors.New("snapshot directory is required")
	}
	dir, err := os.Lstat(snapshot)
	if err != nil {
		return err
	}
	if !dir.IsDir() || dir.Mode().Perm() != 0700 {
		return errors.New("full snapshot root must be a private real directory (0700)")
	}
	content, err := os.ReadDir(snapshot)
	if err != nil {
		return err
	}
	if len(content) != 2 || content[0].Name() != fullSnapshotDataDirName ||
		content[1].Name() != fullSnapshotManifestName {
		return errors.New("full snapshot contains unrecognized or missing root files")
	}
	dataDir := filepath.Join(snapshot, fullSnapshotDataDirName)
	dataInfo, err := os.Lstat(dataDir)
	if err != nil || !dataInfo.IsDir() || dataInfo.Mode().Perm() != 0700 {
		return errors.New("invalid private snapshot data directory")
	}
	manifestPath := filepath.Join(snapshot, fullSnapshotManifestName)
	manifestInfo, err := os.Lstat(manifestPath)
	if err != nil || !manifestInfo.Mode().IsRegular() || manifestInfo.Mode().Perm() != 0600 ||
		manifestInfo.Size() > fullSnapshotManifestMaxBytes {
		return errors.New("invalid or oversized private snapshot manifest")
	}
	raw, err := backupReadPrivateFile(snapshot, fullSnapshotManifestName)
	if err != nil {
		return err
	}
	var manifest fullDataSnapshotManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	if manifest.Version != 1 || manifest.CapturedAt.IsZero() ||
		manifest.CapturedAt.After(time.Now().UTC().Add(5*time.Minute)) ||
		manifest.RootMode > 0777 {
		return errors.New("invalid full snapshot version, source mode or capture time")
	}
	expected := make(map[string]fullDataSnapshotEntry, len(manifest.Entries))
	for _, item := range manifest.Entries {
		relative := filepath.FromSlash(item.Path)
		if !filepath.IsLocal(relative) || relative == "." ||
			filepath.ToSlash(filepath.Clean(relative)) != item.Path ||
			item.Mode > 0777 {
			return fmt.Errorf("unsafe full snapshot manifest path or permissions: %q", item.Path)
		}
		if _, exists := expected[item.Path]; exists {
			return fmt.Errorf("duplicate snapshot entry: %s", item.Path)
		}
		if item.Type == "dir" {
			if item.Size != 0 || item.SHA256 != "" {
				return fmt.Errorf("invalid directory metadata: %s", item.Path)
			}
		} else if item.Type == "file" {
			if item.Size < 0 || len(item.SHA256) != 64 {
				return fmt.Errorf("invalid snapshot file size/digest: %s", item.Path)
			}
		} else {
			return fmt.Errorf("unknown snapshot entry type: %s", item.Path)
		}
		expected[item.Path] = item
	}
	visited := 0
	err = filepath.WalkDir(dataDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dataDir {
			return nil
		}
		rel, err := filepath.Rel(dataDir, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		item, ok := expected[key]
		if !ok {
			return fmt.Errorf("unlisted snapshot path: %s", key)
		}
		stat, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if item.Type == "dir" {
			if !stat.IsDir() || stat.Mode().Perm() != 0700 {
				return fmt.Errorf("invalid snapshot directory type or privacy: %s", key)
			}
		} else {
			if !stat.Mode().IsRegular() || stat.Mode().Perm() != 0600 {
				return fmt.Errorf("invalid snapshot file type or privacy: %s", key)
			}
			digest, size, err := backupFileDigest(filepath.Dir(path), filepath.Base(path))
			if err != nil {
				return err
			}
			if digest != item.SHA256 || size != item.Size {
				return fmt.Errorf("snapshot SHA-256/size mismatch: %s", key)
			}
		}
		visited++
		return nil
	})
	if err != nil {
		return err
	}
	if visited != len(expected) {
		return errors.New("snapshot entries missing from filesystem")
	}
	return nil
}

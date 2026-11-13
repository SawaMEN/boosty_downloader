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
	"sort"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// A full data restore ALWAYS creates a new directory and never modifies an
// active installation. Host /etc config, external TLS keys and ownership are
// outside this detached recovery operation.
type fullDataRestoreHooks struct {
	copyData   func(io.Writer, io.Reader) (int64, error)
	checkpoint func(string) error
}

func RestoreFullDataSnapshot(snapshot, output string) error {
	return restoreFullDataSnapshotWithHooks(snapshot, output, nil)
}

func restoreFullDataSnapshotWithHooks(snapshot, output string, hooks *fullDataRestoreHooks) error {
	if snapshot == "" || output == "" {
		return errors.New("verified full snapshot and new destination are required")
	}
	snapshotAbs, err := filepath.Abs(snapshot)
	if err != nil {
		return err
	}
	snapshotPhysical, err := filepath.EvalSymlinks(snapshotAbs)
	if err != nil {
		return err
	}
	// Pin manifest bytes BEFORE independent verification; a writer racing
	// validation must never supply a different, unvalidated path list.
	raw, err := backupReadPrivateFile(snapshotAbs, fullSnapshotManifestName)
	if err != nil {
		return err
	}
	manifestHash := sha256.Sum256(raw)
	if err := VerifyFullDataSnapshot(snapshotAbs); err != nil {
		return fmt.Errorf("unusable full data snapshot: %w", err)
	}
	if err := fullRestoreManifestUnchanged(snapshotAbs, manifestHash); err != nil {
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
	dest = filepath.Join(parent, filepath.Base(dest))
	if dest == parent {
		return errors.New("restore destination must be a new child of an existing directory")
	}
	rel, err := filepath.Rel(snapshotPhysical, dest)
	if err != nil {
		return err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return errors.New("restore destination must be outside the verified snapshot")
	}
	if _, err := os.Lstat(dest); err == nil {
		return errors.New("restore destination already exists; refusing overwrite")
	} else if !os.IsNotExist(err) {
		return err
	}

	var manifest fullDataSnapshotManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return err
	}

	stage, err := os.MkdirTemp(parent, ".boosty-full-restore-*")
	if err != nil {
		return err
	}
	// This private staging directory may be left after SIGKILL. It is NEVER
	// published as the target and is safe to inspect/remove offline.
	defer os.RemoveAll(stage)
	entries := append([]fullDataSnapshotEntry(nil), manifest.Entries...)
	sort.Slice(entries, func(i, j int) bool {
		di := strings.Count(entries[i].Path, "/")
		dj := strings.Count(entries[j].Path, "/")
		if di != dj {
			return di < dj
		}
		return entries[i].Path < entries[j].Path
	})
	directories := []string{stage}
	dataDir := filepath.Join(snapshotAbs, fullSnapshotDataDirName)
	copyData := io.Copy
	if hooks != nil && hooks.copyData != nil {
		copyData = hooks.copyData
	}
	for _, item := range entries {
		if !filepath.IsLocal(item.Path) || filepath.ToSlash(filepath.Clean(filepath.FromSlash(item.Path))) != item.Path {
			return fmt.Errorf("untrusted snapshot manifest path: %q", item.Path)
		}
		p := filepath.Join(stage, filepath.FromSlash(item.Path))
		if item.Type == "dir" {
			if err := os.Mkdir(p, 0700); err != nil {
				return err
			}
			directories = append(directories, p)
			continue
		}
		if item.Type != "file" {
			return fmt.Errorf("unrecognized snapshot entry: %s", item.Path)
		}
		if err := fullRestoreCopyVerified(dataDir, item, p, copyData); err != nil {
			return fmt.Errorf("cannot restore %s: %w", item.Path, err)
		}
		if hooks != nil && hooks.checkpoint != nil {
			if err := hooks.checkpoint("copied"); err != nil {
				return err
			}
		}
	}
	if err := fullRestoreVerifyTree(stage, manifest.Entries); err != nil {
		return fmt.Errorf("staged restore verification failed: %w", err)
	}
	// Reread the SOURCE after copying. An uncooperative writer may not honor
	// the app's lock, but a changed source must never silently be published.
	if err := VerifyFullDataSnapshot(snapshotAbs); err != nil {
		return fmt.Errorf("snapshot changed during restore: %w", err)
	}
	if err := fullRestoreManifestUnchanged(snapshotAbs, manifestHash); err != nil {
		return err
	}
	if hooks != nil && hooks.checkpoint != nil {
		if err := hooks.checkpoint("verified"); err != nil {
			return err
		}
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := syncMetadataDirectory(directories[i]); err != nil {
			return err
		}
	}
	// RENAME_NOREPLACE is essential: plain os.Rename on Unix can replace an
	// empty directory created by another process between Lstat and promotion.
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, dest, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("cannot publish restore without overwriting destination: %w", err)
	}
	if err := syncMetadataDirectory(parent); err != nil {
		return fmt.Errorf("restored directory published at %s, but parent sync failed: %w", dest, err)
	}
	return nil
}

func fullRestoreManifestUnchanged(snapshot string, pinned [32]byte) error {
	raw, err := backupReadPrivateFile(snapshot, fullSnapshotManifestName)
	if err != nil {
		return err
	}
	if sha256.Sum256(raw) != pinned {
		return errors.New("snapshot manifest changed during restore")
	}
	return nil
}

// Walk the verified snapshot with O_NOFOLLOW for every component beneath its
// data root. A race that turns a parent into a symlink must not read external
// secrets even after the initial full-snapshot validation.
func fullRestoreOpenRegular(base, relative string) (*os.File, error) {
	if !filepath.IsLocal(relative) || strings.Contains(relative, "\\") {
		return nil, errors.New("unsafe recovery path")
	}
	rootBefore, err := os.Lstat(base)
	if err != nil || !rootBefore.IsDir() {
		return nil, errors.New("unsafe recovery data root")
	}
	root, err := os.Open(base)
	if err != nil {
		return nil, err
	}
	rootOpened, err := root.Stat()
	if err != nil || !os.SameFile(rootBefore, rootOpened) {
		root.Close()
		return nil, errors.New("recovery root changed during open")
	}
	parentFD := int(root.Fd())
	ownedFD := -1
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			if ownedFD >= 0 {
				syscall.Close(ownedFD)
			}
			root.Close()
			return nil, errors.New("unsafe recovery component")
		}
		flags := syscall.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= syscall.O_DIRECTORY
		} else {
			flags |= syscall.O_NONBLOCK
		}
		next, openErr := syscall.Openat(parentFD, part, flags, 0)
		if ownedFD >= 0 {
			syscall.Close(ownedFD)
			ownedFD = -1
		}
		if openErr != nil {
			root.Close()
			return nil, openErr
		}
		ownedFD = next
		parentFD = next
	}
	root.Close()
	file := os.NewFile(uintptr(ownedFD), relative)
	if file == nil {
		syscall.Close(ownedFD)
		return nil, errors.New("cannot open recovery file")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		file.Close()
		return nil, errors.New("recovery file is not a private regular file")
	}
	return file, nil
}

func fullRestoreCopyVerified(dataDir string, item fullDataSnapshotEntry, target string, copyData func(io.Writer, io.Reader) (int64, error)) error {
	source, err := fullRestoreOpenRegular(dataDir, filepath.FromSlash(item.Path))
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || info.Size() != item.Size {
		return errors.New("snapshot source size changed")
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, copyErr := copyData(io.MultiWriter(output, hash), source)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	if closeErr := output.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	if size != item.Size || hex.EncodeToString(hash.Sum(nil)) != item.SHA256 {
		return errors.New("restored file differs from verified snapshot SHA-256")
	}
	return nil
}

func fullRestoreVerifyTree(root string, entries []fullDataSnapshotEntry) error {
	expected := make(map[string]fullDataSnapshotEntry, len(entries))
	for _, item := range entries {
		expected[item.Path] = item
	}
	seen := 0
	err := filepath.WalkDir(root, func(path string, dirEntry os.DirEntry, walkErr error) error {
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
		item, ok := expected[key]
		if !ok {
			return fmt.Errorf("unlisted staged file: %s", key)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if item.Type == "dir" {
			if !info.IsDir() || info.Mode().Perm() != 0700 {
				return fmt.Errorf("unsafe restored directory: %s", key)
			}
		} else {
			if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
				return fmt.Errorf("unsafe restored file: %s", key)
			}
			digest, size, err := backupFileDigest(filepath.Dir(path), filepath.Base(path))
			if err != nil {
				return err
			}
			if digest != item.SHA256 || size != item.Size {
				return fmt.Errorf("restored SHA-256 mismatch: %s", key)
			}
		}
		seen++
		return nil
	})
	if err != nil {
		return err
	}
	if seen != len(entries) {
		return errors.New("staged restore is missing snapshot files")
	}
	return nil
}

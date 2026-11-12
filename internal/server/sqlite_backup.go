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

// metadataBackupManifest describes a private recovery point for the JSON/SQL
// metadata only. It never implies that library and revisions media bytes were
// backed up; those need their own filesystem snapshot before a live cutover.
type metadataBackupManifest struct {
	Version    int                  `json:"version"`
	CapturedAt time.Time            `json:"capturedAt"`
	Files      []metadataBackupFile `json:"files"`
}

type metadataBackupFile struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func metadataBackupSource(root, name, dest string) (metadataBackupFile, error) {
	input := filepath.Join(root, name)
	before, err := os.Lstat(input)
	if err != nil {
		return metadataBackupFile{}, err
	}
	if !before.Mode().IsRegular() {
		return metadataBackupFile{}, fmt.Errorf("metadata source is not a regular file: %s", name)
	}
	source, err := os.Open(input)
	if err != nil {
		return metadataBackupFile{}, err
	}
	defer source.Close()
	opened, err := source.Stat()
	if err != nil {
		return metadataBackupFile{}, err
	}
	if !os.SameFile(before, opened) {
		return metadataBackupFile{}, fmt.Errorf("source changed during open: %s", name)
	}
	output, err := os.OpenFile(filepath.Join(dest, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return metadataBackupFile{}, err
	}
	hash := sha256.New()
	count, copyErr := io.Copy(io.MultiWriter(output, hash), source)
	if copyErr == nil {
		copyErr = output.Sync()
	}
	if err := output.Close(); copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		return metadataBackupFile{}, copyErr
	}
	after, err := os.Lstat(input)
	if err != nil {
		return metadataBackupFile{}, err
	}
	if !os.SameFile(opened, after) || before.Size() != count ||
		after.Size() != count || !before.ModTime().Equal(after.ModTime()) {
		return metadataBackupFile{}, fmt.Errorf("source changed during backup: %s", name)
	}
	if strings.HasSuffix(name, ".json") {
		data, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil || !json.Valid(data) {
			return metadataBackupFile{}, fmt.Errorf("invalid backed up JSON: %s", name)
		}
	}
	return metadataBackupFile{
		Name: name, Size: count, SHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

// PrepareSQLiteBackup creates a *new*, private and checksum-verified metadata
// recovery directory for a stopped server. SQLite must first match the JSON
// source. Any existing destination, pending journal, or non-empty WAL is a
// hard error; the operation never edits source archives or changes backends.
func PrepareSQLiteBackup(root, output string) (err error) {
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	originalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("metadata source root must be an existing real directory")
	}
	lock, err := lockDataDirectory(root)
	if err != nil {
		return err
	}
	defer lock.Close()
	if output == "" {
		return errors.New("backup destination must be supplied")
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
	rel, err := filepath.Rel(originalRoot, physicalDest)
	if err != nil {
		return err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return errors.New("backup destination cannot be inside the live data directory")
	}
	if _, err := os.Lstat(filepath.Join(root, metadataJournalName)); err == nil {
		return errors.New("pending JSON metadata transaction: restart to recover before backup")
	} else if !os.IsNotExist(err) {
		return err
	}
	// Raw copying a WAL-mode SQLite file without its uncheckpointed WAL would
	// make a backup internally inconsistent. Fail rather than copy partial SQL.
	wal := filepath.Join(root, "metadata.db-wal")
	if stat, err := os.Lstat(wal); err == nil {
		if !stat.Mode().IsRegular() || stat.Size() != 0 {
			return errors.New("SQLite WAL is not empty: checkpoint with the stopped server before backup")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := verifySQLiteExport(root); err != nil {
		return fmt.Errorf("SQLite export preflight rejected backup: %w", err)
	}
	if err := os.Mkdir(dest, 0700); err != nil {
		return err // never overwrite any directory, even an empty one
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(dest) // only this invocation's newly created directory
		}
	}()
	names := []string{"jobs.json", "imports.json", "authors.json", "accounts.json", "metadata.db"}
	manifest := metadataBackupManifest{Version: 1, CapturedAt: time.Now().UTC()}
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(root, name)); os.IsNotExist(err) {
			if name == "jobs.json" || name == "imports.json" || name == "metadata.db" {
				return fmt.Errorf("required metadata file missing: %s", name)
			}
			continue
		} else if err != nil {
			return err
		}
		file, err := metadataBackupSource(root, name, dest)
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, file)
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := writeSyncedMetadata(dest, "backup-manifest.json", manifestJSON); err != nil {
		return err
	}
	// Reread JSON/SQLite after backup: concurrent writers must invalidate
	// the snapshot rather than result in an apparently valid recovery point.
	if err := verifySQLiteExport(root); err != nil {
		return fmt.Errorf("metadata changed while backing up: %w", err)
	}
	for _, file := range manifest.Files {
		input, err := os.Open(filepath.Join(root, file.Name))
		if err != nil {
			return err
		}
		hash := sha256.New()
		n, checkErr := io.Copy(hash, input)
		if err := input.Close(); checkErr == nil {
			checkErr = err
		}
		if checkErr != nil {
			return checkErr
		}
		if n != file.Size || hex.EncodeToString(hash.Sum(nil)) != file.SHA256 {
			return fmt.Errorf("metadata changed while backing up: %s", file.Name)
		}
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

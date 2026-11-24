package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PrepareSQLiteRecoveryBundle prepares two verified recovery points without
// selecting SQL-primary. Stop all writers for the entire operation; each
// existing backup helper acquires its own OS lock, and the final locked
// preflight rejects any inconsistency between their captured generations.
func PrepareSQLiteRecoveryBundle(root, output string) error {
	if root == "" || output == "" {
		return errors.New("stopped data directory and new bundle directory required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() {
		return errors.New("bundle source must be an existing real directory")
	}
	root, err = filepath.EvalSymlinks(root)
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
	dest = filepath.Join(parent, filepath.Base(dest))
	overlaps := func(a, b string) bool {
		rel, err := filepath.Rel(a, b)
		return err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
	}
	if overlaps(root, dest) || overlaps(dest, root) {
		return errors.New("bundle and source must not overlap")
	}
	check := func() error {
		if err := sqliteRequireOfflineJSON(root); err != nil {
			return err
		}
		if err := sqliteReindexNoWAL(root); err != nil {
			return err
		}
		if _, err := os.Lstat(filepath.Join(root, "boosty-auth.enc")); err == nil {
			return errors.New("legacy authorization must be migrated before SQL activation")
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := sqliteHistoryDeleteGuard(root, false); err != nil {
			return err
		}
		return verifySQLPrimaryAccounts(root)
	}
	lock, err := lockDataDirectory(root)
	if err != nil {
		return err
	}
	err = check()
	lock.Close()
	if err != nil {
		return err
	}
	if err := os.Mkdir(dest, 0700); err != nil {
		return err
	}
	// Preserve completed recovery points even if a later step fails.
	backup, full := filepath.Join(dest, "metadata"), filepath.Join(dest, "full")
	if err := PrepareSQLiteBackup(root, backup); err != nil {
		return fmt.Errorf("metadata backup: %w", err)
	}
	if err := PrepareFullDataSnapshot(root, full); err != nil {
		return fmt.Errorf("full snapshot: %w", err)
	}
	lock, err = lockDataDirectory(root)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := check(); err != nil {
		return err
	}
	if err := verifySQLiteCutoverPreflightLocked(root, backup); err != nil {
		return fmt.Errorf("metadata preflight: %w", err)
	}
	if _, err := verifyFullCutoverSnapshotLocked(root, full, false); err != nil {
		return fmt.Errorf("full snapshot preflight: %w", err)
	}
	if err := syncMetadataDirectory(dest); err != nil {
		return err
	}
	return syncMetadataDirectory(parent)
}

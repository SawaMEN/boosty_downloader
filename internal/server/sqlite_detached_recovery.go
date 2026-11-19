package server

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// detachedSQLRestoreHooks is only used by fault-injection tests; production
// callers cannot skip verification or authorize live backend replacement.
type detachedSQLRestoreHooks struct {
	checkpoint func(string) error
}

// RestoreDetachedSQLRecovery overlays a verified newer SQL->JSON metadata
// export onto a verified full DATA snapshot, then publishes a NEW detached
// directory. It refuses to rewrite the running installation, even on failure.
// This is not SQL-primary activation or automatic in-place rollback.
func RestoreDetachedSQLRecovery(fullSnapshot, sqlExport, output string) error {
	return restoreDetachedSQLRecoveryWithHooks(fullSnapshot, sqlExport, output, nil)
}

func restoreDetachedSQLRecoveryWithHooks(fullSnapshot, sqlExport, output string, hooks *detachedSQLRestoreHooks) error {
	if fullSnapshot == "" || sqlExport == "" || output == "" {
		return errors.New("full data snapshot, verified SQL/JSON export and new destination are all required")
	}
	fullAbs, err := filepath.Abs(fullSnapshot)
	if err != nil {
		return err
	}
	sqlAbs, err := filepath.Abs(sqlExport)
	if err != nil {
		return err
	}
	fullPhysical, err := filepath.EvalSymlinks(fullAbs)
	if err != nil {
		return err
	}
	sqlPhysical, err := filepath.EvalSymlinks(sqlAbs)
	if err != nil {
		return err
	}
	if err := detachedRestoreDirectory(fullAbs); err != nil {
		return fmt.Errorf("unsafe full snapshot path: %w", err)
	}
	if err := detachedRestoreDirectory(sqlAbs); err != nil {
		return fmt.Errorf("unsafe SQL export path: %w", err)
	}
	fullManifest, err := backupReadPrivateFile(fullAbs, fullSnapshotManifestName)
	if err != nil {
		return err
	}
	fullHash := sha256.Sum256(fullManifest)
	sqlManifest, err := backupReadPrivateFile(sqlAbs, "backup-manifest.json")
	if err != nil {
		return err
	}
	sqlHash := sha256.Sum256(sqlManifest)
	if err := VerifyFullDataSnapshot(fullAbs); err != nil {
		return fmt.Errorf("full source snapshot rejected: %w", err)
	}
	if err := VerifySQLiteMetadataBackup(sqlAbs); err != nil {
		return fmt.Errorf("SQL recovery export rejected: %w", err)
	}
	if err := fullRestoreManifestUnchanged(fullAbs, fullHash); err != nil {
		return err
	}
	if err := detachedSQLManifestUnchanged(sqlAbs, sqlHash); err != nil {
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
	for _, source := range []string{fullPhysical, sqlPhysical} {
		rel, err := filepath.Rel(source, dest)
		if err != nil {
			return err
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
			return errors.New("detached restore destination must be outside both source snapshots")
		}
	}
	if _, err := os.Lstat(dest); err == nil {
		return errors.New("detached restore destination already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	if hooks != nil && hooks.checkpoint != nil {
		if err := hooks.checkpoint("validated"); err != nil {
			return err
		}
	}
	// A private parent reserves a staging namespace. A killed process may
	// leave this private container, but never publishes an incomplete output.
	work, err := os.MkdirTemp(parent, ".boosty-sql-recovery-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	stage := filepath.Join(work, "data")
	if err := RestoreFullDataSnapshot(fullAbs, stage); err != nil {
		return err
	}
	for _, journal := range []string{metadataJournalName, sqliteCutoverIntentName} {
		if _, err := os.Lstat(filepath.Join(stage, journal)); err == nil {
			return fmt.Errorf("restored source contains pending journal %s", journal)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := sqliteReindexNoWAL(stage); err != nil {
		return err
	}
	// Account identity and author preferences are not represented by SQL.
	// A newer account store must be restored using a newer FULL snapshot,
	// rather than combining old credentials with newer SQL sharing rules.
	for _, name := range []string{"accounts.json", "authors.json"} {
		if err := detachedSQLCompareOptional(stage, sqlAbs, name); err != nil {
			return fmt.Errorf("cannot reconcile %s across recovery sources: %w", name, err)
		}
	}
	for _, name := range []string{"jobs.json", "imports.json"} {
		raw, err := backupReadPrivateFile(sqlAbs, name)
		if err != nil {
			return err
		}
		if err := writeSyncedMetadata(stage, name, raw); err != nil {
			return err
		}
	}
	sqlPath := filepath.Join(stage, "metadata.db")
	if oldInfo, err := os.Lstat(sqlPath); err != nil || !oldInfo.Mode().IsRegular() {
		return fmt.Errorf("full snapshot lacks a regular SQLite database: %w", err)
	}
	if err := os.Remove(sqlPath); err != nil {
		return err
	}
	if _, err := metadataBackupSource(sqlAbs, "metadata.db", stage); err != nil {
		return err
	}
	if err := syncMetadataDirectory(stage); err != nil {
		return err
	}
	if hooks != nil && hooks.checkpoint != nil {
		if err := hooks.checkpoint("overlaid"); err != nil {
			return err
		}
	}
	if err := verifySQLiteExport(stage); err != nil {
		return fmt.Errorf("recovered JSON and SQL are inconsistent: %w", err)
	}
	if err := verifySQLiteSnapshotInventory(stage); err != nil {
		return fmt.Errorf("recovered media and SQL index disagree: %w", err)
	}
	// Verify sources once again after the entire recovery, not just before
	// copying. A modified source or manifest cannot redefine the baseline.
	if err := VerifyFullDataSnapshot(fullAbs); err != nil {
		return fmt.Errorf("full snapshot changed while restoring: %w", err)
	}
	if err := VerifySQLiteMetadataBackup(sqlAbs); err != nil {
		return fmt.Errorf("SQL snapshot changed while restoring: %w", err)
	}
	if err := fullRestoreManifestUnchanged(fullAbs, fullHash); err != nil {
		return err
	}
	if err := detachedSQLManifestUnchanged(sqlAbs, sqlHash); err != nil {
		return err
	}
	if hooks != nil && hooks.checkpoint != nil {
		if err := hooks.checkpoint("verified"); err != nil {
			return err
		}
	}
	if err := syncMetadataDirectory(stage); err != nil {
		return err
	}
	if err := syncMetadataDirectory(work); err != nil {
		return err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, dest, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("detached SQL recovery cannot overwrite destination: %w", err)
	}
	if err := syncMetadataDirectory(parent); err != nil {
		return fmt.Errorf("detached recovery published at %s but parent sync failed: %w", dest, err)
	}
	return nil
}

func detachedRestoreDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("source must be a real private directory")
	}
	return nil
}

func detachedSQLManifestUnchanged(dir string, pinned [32]byte) error {
	raw, err := backupReadPrivateFile(dir, "backup-manifest.json")
	if err != nil {
		return err
	}
	if sha256.Sum256(raw) != pinned {
		return errors.New("SQL export manifest changed during detached restore")
	}
	return nil
}

func detachedSQLCompareOptional(fullData, sqlExport, name string) error {
	full, fullErr := backupReadPrivateFile(fullData, name)
	fromSQL, sqlErr := backupReadPrivateFile(sqlExport, name)
	if os.IsNotExist(fullErr) && os.IsNotExist(sqlErr) {
		return nil
	}
	if fullErr != nil || sqlErr != nil {
		return errors.New("account/author file presence differs or file is unsafe")
	}
	if !bytes.Equal(full, fromSQL) {
		return errors.New("account/author metadata changed after the full data snapshot")
	}
	return nil
}

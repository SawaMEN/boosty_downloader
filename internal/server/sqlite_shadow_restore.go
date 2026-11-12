package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// RestoreSQLiteShadowFromBackup repairs only the detached, non-authoritative
// metadata.db. It never rewinds JSON ACLs, accounts, media, tokens or TLS.
// The live archive directory must be stopped and identical to the verified
// recovery point except for the SQLite file itself.
func RestoreSQLiteShadowFromBackup(root, backup string) error {
	if root == "" || backup == "" {
		return errors.New("data directory and verified metadata backup are required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	lock, err := lockDataDirectory(root)
	if err != nil {
		return err
	}
	defer lock.Close()

	if err := sqliteReindexNoWAL(root); err != nil {
		return fmt.Errorf("restore blocked by unfinished transaction: %w", err)
	}
	// A checkpointed SQLite database may retain a regular -shm sidecar
	// after a clean close. It contains no committed pages without the WAL;
	// do not reject a verified empty/absent WAL solely because -shm exists.
	// Reject special/symlink sidecars and any rollback journal instead.
	if info, err := os.Lstat(filepath.Join(root, "metadata.db-shm")); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("unsafe SQLite shared-memory sidecar")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(filepath.Join(root, "metadata.db-journal")); err == nil {
		return errors.New("restore requires a closed SQLite database without a rollback journal")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := VerifySQLiteMetadataBackup(backup); err != nil {
		return fmt.Errorf("unusable metadata backup: %w", err)
	}
	if err := sqliteBackupJSONMatches(root, backup); err != nil {
		return fmt.Errorf("cannot restore SQL over changed authoritative JSON: %w", err)
	}
	if err := sqliteRestoreAuditMedia(root, backup); err != nil {
		return fmt.Errorf("recovery point does not match on-disk archives: %w", err)
	}
	// Third-party writers do not honor flock: recheck all source JSON after
	// the potentially long SHA-256 inventory and again before promotion.
	if err := sqliteBackupJSONMatches(root, backup); err != nil {
		return fmt.Errorf("JSON changed during recovery audit: %w", err)
	}

	dbPath := filepath.Join(root, "metadata.db")
	old, err := os.Lstat(dbPath)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if exists && !old.Mode().IsRegular() {
		return errors.New("refusing to overwrite non-regular SQLite database")
	}

	staged, err := os.CreateTemp(root, ".sqlite-shadow-restoring-*")
	if err != nil {
		return err
	}
	stagePath := staged.Name()
	defer os.Remove(stagePath)
	if err := staged.Chmod(0600); err != nil {
		staged.Close()
		return err
	}
	copyErr := sqliteRestoreCopyVerified(backup, staged)
	if err := staged.Close(); copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		return fmt.Errorf("cannot stage verified SQLite recovery: %w", copyErr)
	}
	if err := sqliteBackupJSONMatches(root, backup); err != nil {
		return fmt.Errorf("JSON changed while staging SQLite recovery: %w", err)
	}

	// Retain the original inode via a private same-filesystem hardlink until
	// after the replacement is re-audited. Atomic rename never exposes a
	// partial database, even if the process terminates during promotion.
	previous := ""
	if exists {
		placeholder, err := os.CreateTemp(root, ".sqlite-shadow-previous-*")
		if err != nil {
			return err
		}
		previous = placeholder.Name()
		if err := placeholder.Close(); err != nil {
			os.Remove(previous)
			return err
		}
		if err := os.Remove(previous); err != nil {
			return err
		}
		if err := os.Link(dbPath, previous); err != nil {
			return err
		}
		if now, err := os.Lstat(dbPath); err != nil || !os.SameFile(now, old) {
			os.Remove(previous)
			return errors.New("SQLite database changed before atomic restore")
		}
	}
	if err := os.Rename(stagePath, dbPath); err != nil {
		if previous != "" {
			os.Remove(previous)
		}
		return err
	}
	verify := syncMetadataDirectory(root)
	if verify == nil {
		verify = verifySQLiteExport(root)
	}
	if verify == nil {
		verify = verifySQLiteSnapshotInventory(root)
	}
	if verify != nil {
		// Post-promotion failure must not silently leave a supposedly healthy
		// index. Revert to exactly the previous inode when possible.
		var rollback error
		if previous != "" {
			rollback = os.Rename(previous, dbPath)
		} else {
			rollback = os.Remove(dbPath)
		}
		if rollback != nil {
			return fmt.Errorf("SQLite restore verification failed: %v; previous SQLite preserved at %s; rollback failed: %w", verify, previous, rollback)
		}
		if err := syncMetadataDirectory(root); err != nil {
			return fmt.Errorf("SQLite restore rejected: %v; previous SQL reverted but directory sync failed: %w", verify, err)
		}
		return fmt.Errorf("SQLite restore rejected and previous SQL restored: %w", verify)
	}
	if previous != "" {
		if err := os.Remove(previous); err != nil {
			return fmt.Errorf("SQLite restored, but old inode cleanup failed at %s: %w", previous, err)
		}
		if err := syncMetadataDirectory(root); err != nil {
			return fmt.Errorf("SQLite restored, but cleanup directory sync failed: %w", err)
		}
	}
	return nil
}

func sqliteRestoreAuditMedia(root, backup string) error {
	db, err := openSQLiteReadOnly(filepath.Join(backup, "metadata.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := verifySQLiteSnapshots(root, db); err != nil {
		return err
	}
	if err := sqliteAuditInventory(root, db); err != nil {
		return err
	}
	if err := sqliteAuditUnindexedSnapshots(root, db); err != nil {
		return err
	}
	return sqliteAuditCAS(root, db)
}

// The private backup was checked before copying; compare the streamed bytes
// with the current backup digest again in case an external tool changed it.
func sqliteRestoreCopyVerified(backup string, target *os.File) error {
	original := filepath.Join(backup, "metadata.db")
	beforeHash, beforeSize, err := backupFileDigest(backup, "metadata.db")
	if err != nil {
		return err
	}
	source, err := os.Open(original)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(target, hash), source)
	if err := source.Close(); copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		return copyErr
	}
	if size != beforeSize || hex.EncodeToString(hash.Sum(nil)) != beforeHash {
		return errors.New("backup SQLite changed while being copied")
	}
	afterHash, afterSize, err := backupFileDigest(backup, "metadata.db")
	if err != nil {
		return err
	}
	if afterHash != beforeHash || afterSize != beforeSize {
		return errors.New("backup SQLite changed after being copied")
	}
	return target.Sync()
}

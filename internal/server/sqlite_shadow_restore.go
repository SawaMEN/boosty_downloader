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
)

// RestoreSQLiteShadowFromBackup repairs only the detached, non-authoritative
// metadata.db. It never rewinds JSON ACLs, accounts, media, tokens or TLS.
// The live archive directory must be stopped and identical to the verified
// recovery point except for the SQLite file itself.
// restoreSQLiteShadowHooks enables deterministic fault injection in tests
// without exposing unsafe crash flags or bypasses through the production CLI.
type restoreSQLiteShadowHooks struct {
	copyData   func(io.Writer, io.Reader) (int64, error)
	checkpoint func(string) error
}

// Pin recovery identity from the manifest BEFORE verifying SQLite.
// A replacement after verification must never become the new baseline.
type sqliteRestoreSource struct {
	hash         string
	size         int64
	manifestHash [32]byte
}

func RestoreSQLiteShadowFromBackup(root, backup string) error {
	return restoreSQLiteShadowWithHooks(root, backup, nil)
}

func restoreSQLiteShadowWithHooks(root, backup string, hooks *restoreSQLiteShadowHooks) error {
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
	// A backup physically inside the live data directory could be rewritten
	// by the application or mistakenly included in archive maintenance. Do
	// not permit recovery from an in-place or symlink-aliased source.
	livePhysical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	backupPhysical, err := filepath.EvalSymlinks(backup)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(livePhysical, backupPhysical)
	if err != nil {
		return err
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))) {
		return errors.New("SQLite recovery snapshot must be outside the live data directory")
	}

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
	source, err := sqliteRestorePinManifest(backup)
	if err != nil {
		return fmt.Errorf("cannot pin recovery manifest: %w", err)
	}
	if err := VerifySQLiteMetadataBackup(backup); err != nil {
		return fmt.Errorf("unusable metadata backup: %w", err)
	}
	if hooks != nil && hooks.checkpoint != nil {
		if err := hooks.checkpoint("verified"); err != nil {
			return err
		}
	}
	if err := sqliteRestoreBackupUnchanged(backup, source); err != nil {
		return err
	}
	if err := sqliteBackupJSONMatches(root, backup); err != nil {
		return fmt.Errorf("cannot restore SQL over changed authoritative JSON: %w", err)
	}
	if err := sqliteRestoreAuditMedia(root, backup); err != nil {
		return fmt.Errorf("recovery point does not match on-disk archives: %w", err)
	}
	if hooks != nil && hooks.checkpoint != nil {
		if err := hooks.checkpoint("audited"); err != nil {
			return err
		}
	}
	if err := sqliteRestoreBackupUnchanged(backup, source); err != nil {
		return err
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
	copyData := io.Copy
	if hooks != nil && hooks.copyData != nil {
		copyData = hooks.copyData
	}
	copyErr := sqliteRestoreCopyVerified(backup, staged, copyData, source)
	if err := staged.Close(); copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		return fmt.Errorf("cannot stage verified SQLite recovery: %w", copyErr)
	}
	if err := sqliteBackupJSONMatches(root, backup); err != nil {
		return fmt.Errorf("JSON changed while staging SQLite recovery: %w", err)
	}
	if hooks != nil && hooks.checkpoint != nil {
		if err := hooks.checkpoint("staged"); err != nil {
			return err
		}
	}
	if err := sqliteRestoreBackupUnchanged(backup, source); err != nil {
		return err
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
	if verify == nil && hooks != nil && hooks.checkpoint != nil {
		verify = hooks.checkpoint("promoted")
	}
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
	// A process killed between fsync and rename can leave a private staged
	// file. A SIGKILL after rename can leave the old SQL hardlink. Only a
	// newly verified and successful restore may retire those stale files.
	if err := sqliteRestoreCleanupOrphans(root); err != nil {
		return fmt.Errorf("SQLite restored, but stale recovery file cleanup failed: %w", err)
	}
	return nil
}

// Remove only the private temporary names owned by this recovery mechanism.
// Never follow a symlink or delete an unknown/special entry. A failed restore
// leaves old recovery inodes available for manual inspection.
func sqliteRestoreCleanupOrphans(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, ".sqlite-shadow-restoring-") &&
			!strings.HasPrefix(name, ".sqlite-shadow-previous-") {
			continue
		}
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsafe stale SQLite recovery file: %s", name)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return syncMetadataDirectory(root)
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

// Read manifest bytes before verification so later hashing cannot silently
// reset the recovery point. VerifySQLiteMetadataBackup validates the entire
// manifest, every checksum, and JSON/SQLite metadata parity.
func sqliteRestorePinManifest(backup string) (sqliteRestoreSource, error) {
	raw, err := backupReadPrivateFile(backup, "backup-manifest.json")
	if err != nil {
		return sqliteRestoreSource{}, err
	}
	var manifest metadataBackupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return sqliteRestoreSource{}, err
	}
	for _, file := range manifest.Files {
		if file.Name == "metadata.db" {
			return sqliteRestoreSource{
				hash: file.SHA256, size: file.Size, manifestHash: sha256.Sum256(raw),
			}, nil
		}
	}
	return sqliteRestoreSource{}, errors.New("recovery manifest is missing SQLite database")
}

// Compare streamed bytes with the digest captured BEFORE recovery verification.
func sqliteRestoreCopyVerified(backup string, target *os.File, copyData func(io.Writer, io.Reader) (int64, error), pinned sqliteRestoreSource) error {
	original := filepath.Join(backup, "metadata.db")
	if err := sqliteRestoreBackupUnchanged(backup, pinned); err != nil {
		return err
	}
	source, err := os.Open(original)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, copyErr := copyData(io.MultiWriter(target, hash), source)
	if err := source.Close(); copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		return copyErr
	}
	if size != pinned.size || hex.EncodeToString(hash.Sum(nil)) != pinned.hash {
		return errors.New("verified backup SQLite changed while being copied")
	}
	if err := sqliteRestoreBackupUnchanged(backup, pinned); err != nil {
		return err
	}
	return target.Sync()
}

// A verified recovery point includes the manifest as well as metadata.db.
// Reject a replaced manifest even if it supplies matching new SQL checksums.
func sqliteRestoreBackupUnchanged(backup string, pinned sqliteRestoreSource) error {
	raw, err := backupReadPrivateFile(backup, "backup-manifest.json")
	if err != nil {
		return fmt.Errorf("verified backup manifest became inaccessible: %w", err)
	}
	if sha256.Sum256(raw) != pinned.manifestHash {
		return errors.New("verified backup manifest changed during restore")
	}
	hash, size, err := backupFileDigest(backup, "metadata.db")
	if err != nil {
		return fmt.Errorf("verified backup SQLite became inaccessible: %w", err)
	}
	if hash != pinned.hash || size != pinned.size {
		return errors.New("verified backup SQLite changed during restore")
	}
	return nil
}

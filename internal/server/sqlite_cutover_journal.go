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

// This file implements the first durable cutover boundary: an OFFLINE,
// read-only rehearsal with a crash-recoverable intent. It intentionally never
// switches running writes to SQLite. The SQL-primary adapter must be completed
// and audited separately before this intent can have a "committed" phase.
const sqliteCutoverIntentName = ".sqlite-cutover-intent.json"
const sqliteCutoverIntentVersion = 1

type sqliteCutoverIntent struct {
	Version            int       `json:"version"`
	Phase              string    `json:"phase"`
	DataDir            string    `json:"dataDir"`
	BackupDir          string    `json:"backupDir"`
	BackupManifestHash string    `json:"backupManifestSha256"`
	PreparedAt         time.Time `json:"preparedAt"`
}

type sqliteCutoverRehearsalHooks struct {
	checkpoint func(string) error
}

// PrepareSQLiteCutoverRehearsal verifies a stopped installation, pins a
// VERIFIED recovery snapshot and durably records a prepared intent. No JSON,
// SQLite, ACL, media or backend flag is changed by this operation. A future
// server startup can automatically abort this unfinished intent only if the
// pinned backup and live data are still identical.
func PrepareSQLiteCutoverRehearsal(root, backup string) error {
	return prepareSQLiteCutoverRehearsal(root, backup, nil)
}

func prepareSQLiteCutoverRehearsal(root, backup string, hooks *sqliteCutoverRehearsalHooks) error {
	if root == "" || backup == "" {
		return errors.New("live data and external recovery snapshot are required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	lock, err := lockDataDirectory(abs)
	if err != nil {
		return err
	}
	defer lock.Close()

	physicalRoot, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	physicalBackup, err := filepath.EvalSymlinks(backup)
	if err != nil {
		return err
	}
	physicalBackup, err = filepath.Abs(physicalBackup)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(physicalRoot, physicalBackup)
	if err != nil {
		return err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return errors.New("recovery snapshot must be outside the live data directory")
	}
	marker := filepath.Join(abs, sqliteCutoverIntentName)
	if _, err := os.Lstat(marker); err == nil {
		return errors.New("a pending SQLite cutover intent must be recovered before preparing another")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := verifySQLiteCutoverPreflightLocked(abs, physicalBackup); err != nil {
		return fmt.Errorf("SQLite cutover rehearsal rejected: %w", err)
	}
	manifestHash, _, err := backupFileDigest(physicalBackup, "backup-manifest.json")
	if err != nil {
		return err
	}
	intent := sqliteCutoverIntent{
		Version: sqliteCutoverIntentVersion, Phase: "prepared",
		DataDir: physicalRoot, BackupDir: physicalBackup,
		BackupManifestHash: manifestHash, PreparedAt: time.Now().UTC(),
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	if hooks != nil && hooks.checkpoint != nil {
		if err := hooks.checkpoint("validated"); err != nil {
			return err
		}
	}
	// The fsynced intent is the linearization point. An interrupted staging
	// operation leaves a recognizable marker, never a partially enabled
	// backend. On error AFTER publication, keep the journal for recovery.
	if err := writeSyncedMetadata(abs, sqliteCutoverIntentName, raw); err != nil {
		return err
	}
	if hooks != nil && hooks.checkpoint != nil {
		if err := hooks.checkpoint("prepared"); err != nil {
			return err
		}
	}
	return nil
}

// RecoverSQLiteCutoverRehearsal is an explicit offline recovery operation.
// It is also invoked automatically during server startup before workers or
// migrations run. No recovery ever overwrites newer JSON or SQL content.
func RecoverSQLiteCutoverRehearsal(root string) error {
	if root == "" {
		return errors.New("data directory is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	lock, err := lockDataDirectory(abs)
	if err != nil {
		return err
	}
	defer lock.Close()
	return recoverSQLiteCutoverRehearsalLocked(abs)
}

func readSQLiteCutoverIntent(root string) (sqliteCutoverIntent, bool, error) {
	path := filepath.Join(root, sqliteCutoverIntentName)
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return sqliteCutoverIntent{}, false, nil
	}
	if err != nil {
		return sqliteCutoverIntent{}, false, err
	}
	if !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() > 32<<10 {
		return sqliteCutoverIntent{}, false, errors.New("unsafe SQLite cutover intent file")
	}
	f, err := os.Open(path)
	if err != nil {
		return sqliteCutoverIntent{}, false, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return sqliteCutoverIntent{}, false, errors.New("cutover intent changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 32<<10+1))
	if err != nil {
		return sqliteCutoverIntent{}, false, err
	}
	var intent sqliteCutoverIntent
	if err := json.Unmarshal(raw, &intent); err != nil {
		return intent, false, err
	}
	if intent.Version != sqliteCutoverIntentVersion || intent.Phase != "prepared" ||
		intent.PreparedAt.IsZero() || intent.PreparedAt.After(time.Now().UTC().Add(5*time.Minute)) ||
		len(intent.BackupManifestHash) != sha256.Size*2 {
		return intent, false, errors.New("unknown or invalid SQLite cutover phase")
	}
	if _, err := hex.DecodeString(intent.BackupManifestHash); err != nil {
		return intent, false, errors.New("invalid cutover backup digest")
	}
	return intent, true, nil
}

// Lock must be held by the caller. A bad journal, a tampered backup or a
// changed live JSON/SQL/media snapshot leaves the intent in place and blocks
// server startup. This is fail-closed, not destructive auto-restore.
func recoverSQLiteCutoverRehearsalLocked(root string) error {
	intent, found, err := readSQLiteCutoverIntent(root)
	if err != nil || !found {
		return err
	}
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	if intent.DataDir != physicalRoot || !filepath.IsAbs(intent.BackupDir) {
		return errors.New("SQLite cutover intent belongs to another data directory")
	}
	physicalBackup, err := filepath.EvalSymlinks(intent.BackupDir)
	if err != nil || physicalBackup != intent.BackupDir {
		return errors.New("SQLite cutover backup changed its physical location")
	}
	rel, err := filepath.Rel(physicalRoot, physicalBackup)
	if err != nil {
		return err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return errors.New("SQLite cutover backup is inside the data directory")
	}
	manifestHash, _, err := backupFileDigest(physicalBackup, "backup-manifest.json")
	if err != nil {
		return err
	}
	if manifestHash != intent.BackupManifestHash {
		return errors.New("SQLite cutover recovery snapshot manifest has changed")
	}
	if err := verifySQLiteCutoverPreflightLocked(root, physicalBackup); err != nil {
		return fmt.Errorf("pending SQLite cutover cannot be safely canceled: %w", err)
	}
	manifestHash, _, err = backupFileDigest(physicalBackup, "backup-manifest.json")
	if err != nil || manifestHash != intent.BackupManifestHash {
		return errors.New("SQLite cutover snapshot changed during recovery")
	}
	// Removing the durable marker is the ONLY mutation. SQL-primary is still
	// disabled, and the untouched JSON remains the authoritative backend.
	if err := removeSyncedMetadata(root, sqliteCutoverIntentName); err != nil {
		return fmt.Errorf("unable to finalize SQLite cutover cancellation: %w", err)
	}
	return nil
}

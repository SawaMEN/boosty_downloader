package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Shadow repair operations must not replace or reindex a selected primary
// database, even with a byte-identical old recovery point. They also must
// not run during a prepared cutover rehearsal.
func sqliteRequireOfflineJSON(root string) error {
	mode, err := readSQLiteBackendSelection(root)
	if err != nil {
		return err
	}
	if mode != "json" {
		return errors.New("operation requires a JSON-selected detached SQLite index")
	}
	if _, err := os.Lstat(filepath.Join(root, sqliteCutoverIntentName)); err == nil {
		return errors.New("operation blocked by pending SQLite cutover rehearsal")
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Activation changes ONLY the backend selector, so a multi-file intent
// journal is unnecessary. Both recovery points are mandatory and must
// describe the exact stopped installation. The atomic, fsynced selector
// replacement is the commit point; prior to it JSON remains selected.
// ActivateSQLitePrimaryExperimental selects SQLite only while the service is
// stopped. The server still requires BD_SQLITE_PRIMARY=1 on every start.
func ActivateSQLitePrimaryExperimental(root,backup,snapshot string)error{
	return activateSQLPrimaryOffline(root,backup,snapshot,nil)
}

func activateSQLPrimaryOffline(root, backup, snapshot string, checkpoint func(string) error) error {
	if root == "" || backup == "" || snapshot == "" {
		return errors.New("live data, verified metadata backup and full snapshot are required")
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
	if err := sqliteRequireOfflineJSON(root); err != nil {
		return err
	}
	// Refuse legacy token migration and unexpected pending transactions.
	if _, err := os.Lstat(filepath.Join(root, "boosty-auth.enc")); err == nil {
		return errors.New("legacy authorization must be migrated before SQL activation")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := sqliteHistoryDeleteGuard(root, false); err != nil {
		return err
	}
	physicalBackup, err := fullCutoverSnapshotPath(root, backup)
	if err != nil {
		return err
	}
	physicalSnapshot, err := fullCutoverSnapshotPath(root, snapshot)
	if err != nil {
		return err
	}
	if physicalBackup == physicalSnapshot {
		return errors.New("metadata and full recovery snapshots must be distinct")
	}
	if err := verifySQLiteCutoverPreflightLocked(root, physicalBackup); err != nil {
		return fmt.Errorf("SQL activation metadata preflight: %w", err)
	}
	if _, err := verifyFullCutoverSnapshotLocked(root, physicalSnapshot, false); err != nil {
		return fmt.Errorf("SQL activation full snapshot: %w", err)
	}
	// Account state remains in its existing store. Never enable an archive
	// backend which would silently bootstrap a replacement admin account.
	if err:=verifySQLPrimaryAccounts(root);err!=nil{return err}
	if checkpoint != nil {
		if err := checkpoint("validated"); err != nil {return err}
	}
	if err := sqliteRequireOfflineJSON(root); err != nil {return err}
	if err := sqliteHistoryDeleteGuard(root, false); err != nil {return err}
	selector, err := json.Marshal(sqliteBackendSelection{Version: 1, Active: "sqlite"})
	if err != nil {return err}
	if err := writeSyncedMetadata(root, sqliteBackendSelectionName, selector); err != nil {return err}
	if checkpoint != nil {
		if err := checkpoint("selected"); err != nil {return err}
	}
	return nil
}

// Validate before newAccounts can bootstrap an empty or absent account file.
func verifySQLPrimaryAccounts(root string)error{
	raw, err := videoPublicationReadFile(root, "accounts.json", 64<<20)
	if err != nil {
		return fmt.Errorf("SQL activation requires existing account metadata: %w", err)
	}
	var accounts accountFile
	if err := json.Unmarshal(raw, &accounts); err != nil {
		return err
	}
	admin := false
	for id, account := range accounts.Accounts {
		if id != account.ID || !safeArchivePart(id) || account.Password == "" {
			return errors.New("invalid existing account identity")
		}
		if account.Admin {
			admin = true
		}
	}
	if !admin {
		return errors.New("SQL activation requires an existing administrator")
	}
	return nil
}

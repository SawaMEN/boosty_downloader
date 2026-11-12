package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// VerifySQLiteCutoverPreflight is an offline, read-only gate for the SQL
// migration plan. Passing the gate does NOT enable SQL as the main backend;
// it only proves that a verified rollback snapshot, private JSON ACLs, SQL
// metadata and filesystem media currently agree. Stop all writers first.
func VerifySQLiteCutoverPreflight(root, backup string) error {
	if root == "" || backup == "" {
		return errors.New("data directory and private recovery snapshot are both required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("SQLite preflight needs a real, stopped data directory")
	}
	lock, err := lockDataDirectory(absolute)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := sqliteReindexNoWAL(absolute); err != nil {
		return err
	}
	if err := VerifySQLiteMetadataBackup(backup); err != nil {
		return fmt.Errorf("unusable metadata rollback backup: %w", err)
	}
	if err := sqliteReindexSameBackup(absolute, backup); err != nil {
		return fmt.Errorf("metadata backup is not the same recovery point as live data: %w", err)
	}
	if err := verifySQLiteExport(absolute); err != nil {
		return fmt.Errorf("SQL is not an exact owner-qualified JSON projection: %w", err)
	}
	if err := verifySQLiteSnapshotInventory(absolute); err != nil {
		return fmt.Errorf("SQL revision index, CAS and archived bytes differ from disk: %w", err)
	}
	return nil
}

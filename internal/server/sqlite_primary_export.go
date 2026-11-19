package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// ExportSQLiteMetadataToJSON creates a NEW, detached and independently
// verifiable metadata recovery snapshot from SQL. SQL may legitimately be
// newer than JSON; neither live backend, tokens nor media are changed.
// This is a prerequisite for SQL-primary rollback, NOT an automatic cutover.
func ExportSQLiteMetadataToJSON(root, output string) (err error) {
	if root == "" || output == "" {
		return errors.New("data directory and new SQL recovery destination are required")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("SQL recovery export requires a real data directory")
	}
	lock, err := lockDataDirectory(root)
	if err != nil {
		return err
	}
	defer lock.Close()

	for _, name := range []string{metadataJournalName, sqliteCutoverIntentName} {
		if _, statErr := os.Lstat(filepath.Join(root, name)); statErr == nil {
			return fmt.Errorf("cannot export during pending metadata/backend transition: %s", name)
		} else if !os.IsNotExist(statErr) {
			return statErr
		}
	}
	if err := sqliteReindexNoWAL(root); err != nil {
		return err
	}
	if err := verifySQLiteSnapshotInventory(root); err != nil {
		return fmt.Errorf("SQL recovery export inventory audit: %w", err)
	}
	pinnedHash, pinnedSize, err := backupFileDigest(root, "metadata.db")
	if err != nil {
		return err
	}
	db, err := openSQLiteReadOnly(filepath.Join(root, "metadata.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := sqliteAuditDatabaseIntegrity(db); err != nil {
		return err
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	jobs, err := sqliteShadowSQLJobs(ctx, tx)
	if err != nil {
		return err
	}
	imports, err := sqliteShadowSQLImports(ctx, tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return writeDetachedSQLJSONSnapshot(root, output, pinnedHash, pinnedSize, jobs, imports)
}

func writeDetachedSQLJSONSnapshot(root, output, pinnedHash string, pinnedSize int64,
	jobs map[string]Job, imports map[string]AuthorImport) error {
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return err
	}
	output = filepath.Join(parent, filepath.Base(output))
	rel, err := filepath.Rel(physicalRoot, output)
	if err != nil {
		return err
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return errors.New("SQL recovery export must be outside data directory")
	}
	if _, err := os.Lstat(output); err == nil {
		return errors.New("SQL recovery export cannot overwrite an existing destination")
	} else if !os.IsNotExist(err) {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".boosty-sql-json-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0700); err != nil {
		return err
	}
	manifest := metadataBackupManifest{Version: 1, CapturedAt: time.Now().UTC()}
	for _, item := range []struct {
		name string
		data any
	}{
		{"jobs.json", jobs},
		{"imports.json", imports},
	} {
		raw, err := json.MarshalIndent(item.data, "", "  ")
		if err != nil {
			return err
		}
		if err := writeSyncedMetadata(stage, item.name, raw); err != nil {
			return err
		}
		digest, size, err := backupFileDigest(stage, item.name)
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, metadataBackupFile{
			Name: item.name, SHA256: digest, Size: size,
		})
	}
	// Accounts and author preferences are not derivable from SQL. Preserve
	// their existing private copy, without claiming SQL is authoritative.
	for _, name := range []string{"authors.json", "accounts.json", "metadata.db"} {
		if name != "metadata.db" {
			if _, err := os.Lstat(filepath.Join(root, name)); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return err
			}
		}
		entry, err := metadataBackupSource(root, name, stage)
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, entry)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := writeSyncedMetadata(stage, "backup-manifest.json", raw); err != nil {
		return err
	}
	if gotHash, gotSize, err := backupFileDigest(stage, "metadata.db"); err != nil ||
		gotHash != pinnedHash || gotSize != pinnedSize {
		return errors.New("SQL changed while creating recovery export")
	}
	if err := VerifySQLiteMetadataBackup(stage); err != nil {
		return fmt.Errorf("generated SQL/JSON snapshot did not verify: %w", err)
	}
	if err := sqliteReindexNoWAL(root); err != nil {
		return err
	}
	if gotHash, gotSize, err := backupFileDigest(root, "metadata.db"); err != nil ||
		gotHash != pinnedHash || gotSize != pinnedSize {
		return errors.New("live SQL changed during recovery export")
	}
	if err := verifySQLiteSnapshotInventory(root); err != nil {
		return fmt.Errorf("source media changed during SQL recovery export: %w", err)
	}
	// Atomic no-replace publication. SIGKILL can leave only a private
	// unexposed staging directory, never a partially verified destination.
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, output, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("publishing SQL recovery snapshot: %w", err)
	}
	if err := syncMetadataDirectory(parent); err != nil {
		return fmt.Errorf("snapshot published but directory fsync failed: %w", err)
	}
	return nil
}

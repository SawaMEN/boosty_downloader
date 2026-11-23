package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// A durable backend-selection record is a separate deployment decision,
// never implied by an environment flag, presence of metadata.db or shadow
// synchronization. The production server supports only JSON until complete
// SQL-primary startup, shutdown, rollback and all writer paths are audited.
const sqliteBackendSelectionName = ".metadata-backend.json"

type sqliteBackendSelection struct {
	Version int    `json:"version"`
	Active  string `json:"active"`
}

// readSQLiteBackendSelection is read before any JSON migration or worker is
// started. Missing record represents a legacy JSON deployment. An explicit
// but unsupported "sqlite" value is intentionally preserved as data; the
// server must reject it instead of silently falling back to stale JSON.
func readSQLiteBackendSelection(root string) (string, error) {
	path := filepath.Join(root, sqliteBackendSelectionName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "json", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() == 0 || info.Size() > 256 {
		return "", errors.New("unsafe metadata backend selector")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("cannot safely open backend selector: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() ||
		opened.Mode().Perm()&0077 != 0 || opened.Size() == 0 || opened.Size() > 256 {
		return "", errors.New("backend selector was replaced or is unsafe")
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var selected sqliteBackendSelection
	if err := dec.Decode(&selected); err != nil {
		return "", fmt.Errorf("invalid metadata backend selector: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return "", errors.New("metadata backend selector has trailing data")
	}
	if selected.Version != 1 || (selected.Active != "json" && selected.Active != "sqlite") {
		return "", errors.New("unsupported metadata backend selector version or mode")
	}
	return selected.Active, nil
}

// PinJSONBackendSelection writes a private, fsynced, create-only JSON backend
// marker for a stopped server. This NEVER activates SQLite and does not touch
// jobs, grants, media, tokens or SQLite rows. Only a future fully verified
// cutover operation may switch the pinned selector from JSON to SQLite.
func PinJSONBackendSelection(root string) error {
	if root == "" {
		return errors.New("data directory is required")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	lock, err := lockDataDirectory(absolute)
	if err != nil {
		return err
	}
	defer lock.Close()
	active, err := readSQLiteBackendSelection(absolute)
	if err != nil {
		return err
	}
	if active != "json" {
		return errors.New("cannot replace a SQLite-active backend marker with a JSON marker")
	}
	path := filepath.Join(absolute, sqliteBackendSelectionName)
	for _, pending := range []string{metadataJournalName, sqlitePrimaryTransactionName, sqliteCutoverIntentName, purgeJournalName, revisionRestoreJournalName, videoPublicationJournalName, archiveUpdateJournalName, sqliteRollbackJournalName, sqliteHistoryJournalName, "metadata.db-journal", "metadata.db-wal"} {
		if info, err := os.Lstat(filepath.Join(absolute, pending)); err == nil {
			// A clean, zero-byte SQLite WAL sidecar may remain after a
			// read-only checkpoint. Only a real pending WAL blocks pinning.
			if pending == "metadata.db-wal" && info.Mode().IsRegular() && info.Size() == 0 {
				continue
			}
			return fmt.Errorf("cannot pin backend with pending journal: %s", pending)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if _, err := sqliteShadowReadJobs(filepath.Join(absolute, "jobs.json")); err != nil {
		return fmt.Errorf("JSON backend has no complete jobs snapshot: %w", err)
	}
	if _, err := sqliteShadowReadImports(filepath.Join(absolute, "imports.json")); err != nil {
		return fmt.Errorf("JSON backend has no complete import snapshot: %w", err)
	}
	// A detached or shadow SQLite index may hold SQL-only changes, including
	// new grants. Never declare older JSON to be the selected backend until
	// exact JSON/SQL/ACL parity has been verified, or no SQL DB exists.
	if info, err := os.Lstat(filepath.Join(absolute, "metadata.db")); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("SQLite database is not a regular file")
		}
		if err := verifyJSONSQLiteAgreement(absolute); err != nil {
			return fmt.Errorf("cannot pin JSON over divergent SQLite: %w", err)
		}
		if err := verifySQLiteSnapshotInventory(absolute); err != nil {
			return fmt.Errorf("cannot pin JSON over unverified SQL/media inventory: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil // existing verified JSON marker remains untouched
	} else if !os.IsNotExist(err) {
		return err
	}
	raw, err := json.Marshal(sqliteBackendSelection{Version: 1, Active: "json"})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(absolute, ".backend-select-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := unix.Renameat2(unix.AT_FDCWD, temp, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("backend selector cannot replace an existing destination: %w", err)
	}
	return syncMetadataDirectory(absolute)
}

// verifyJSONSQLiteAgreement guards both durable JSON selection and later JSON
// restarts. Once a backend is pinned, a newer SQL-only grant must never be
// overwritten by an unverified JSON startup. Call with the data-dir lock held.
// A legacy JSON deployment without metadata.db is still supported.
func verifyJSONSQLiteAgreement(root string) error {
	sqlPath := filepath.Join(root, "metadata.db")
	info, err := os.Lstat(sqlPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("unsafe SQLite file beside pinned JSON backend")
	}
	if err := verifySQLiteExport(root); err != nil {
		return fmt.Errorf("SQLite/JSON export parity rejected: %w", err)
	}
	jobs, err := sqliteShadowReadJobs(filepath.Join(root, "jobs.json"))
	if err != nil {
		return err
	}
	imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
	if err != nil {
		return err
	}
	db, err := openSQLiteReadOnly(sqlPath)
	if err != nil {
		return err
	}
	parityErr := (&sqliteMetadata{db: db}).verifyJSONParity(context.Background(), jobs, imports)
	closeErr := db.Close()
	if parityErr != nil {
		return fmt.Errorf("pinned JSON differs from indexed SQL metadata/ACL: %w", parityErr)
	}
	return closeErr
}

package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// An SQL-first commit must be recoverable even if SIGKILL happens after the
// SQLite commit but before the JSON mirror is durably published. The journal
// holds exact before/after hashes with row deltas or full legacy images.
// This transaction layer is not yet connected to live Storage writes.
const sqlitePrimaryTransactionName = ".sqlite-primary-transaction.json"
const sqlitePrimaryTransactionVersion = 1
const sqlitePrimaryJournalMaxBytes = 128 << 20

type sqlitePrimaryTransaction struct {
	Version int           `json:"version"`
	Jobs    metadataImage `json:"jobs"`
	Imports metadataImage `json:"imports"`
}

type sqlitePrimaryCommitHooks struct {
	checkpoint func(string) error
	// Internal archive publication may update the current revision index
	// inside the same SQL commit. This never runs after COMMIT.
	reconcileExtra func(context.Context, *sql.Tx) error
}

// sqlitePrimaryCommitMetadata is a candidate SQL-authoritative commit
// boundary. The caller MUST own the data-directory OS lock and serialize
// every job/import writer (Storage.mu). It is deliberately not exposed as an
// option for the production server until all storage paths use it.
//
// A committed SQL transaction is NEVER rolled back over new data. Instead,
// the journal rolls forward the two-file JSON mirror after a crash.
func sqlitePrimaryCommitMetadata(root string, m *sqliteMetadata, jobs map[string]Job,
	imports map[string]AuthorImport, hooks *sqlitePrimaryCommitHooks) error {
	return sqlitePrimaryCommitMetadataWithLimit(root, m, jobs, imports, hooks, sqlitePrimaryJournalMaxBytes)
}

// A smaller limit permits exercising the pre-commit rejection without allocating
// hundreds of MiB in tests. Never allow a journal larger than recovery accepts.
func sqlitePrimaryCommitMetadataWithLimit(root string, m *sqliteMetadata, jobs map[string]Job,
	imports map[string]AuthorImport, hooks *sqlitePrimaryCommitHooks, limit int) (err error) {
	if limit <= 0 || limit > sqlitePrimaryJournalMaxBytes {
		return errors.New("invalid SQL primary journal size limit")
	}
	if root == "" || m == nil {
		return errors.New("SQL primary transaction needs data directory and open SQLite")
	}
	if err := recoverMetadataPair(root); err != nil {
		return fmt.Errorf("previous JSON transaction could not be recovered: %w", err)
	}
	if err := recoverSQLitePrimaryCommitLocked(root); err != nil {
		return fmt.Errorf("previous SQL primary transaction could not be recovered: %w", err)
	}
	afterJobs, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return err
	}
	afterImports, err := json.MarshalIndent(imports, "", "  ")
	if err != nil {
		return err
	}
	jobImage, err := checkedMetadataImage(root, "jobs.json", afterJobs)
	if err != nil {
		return err
	}
	importImage, err := checkedMetadataImage(root, "imports.json", afterImports)
	if err != nil {
		return err
	}
	// SQL-primary activation is gated on a complete earlier JSON/SQL
	// checkpoint. Do not support bootstrap from a missing JSON file here.
	if !jobImage.Existed || !importImage.Existed {
		return errors.New("SQL primary transaction requires complete existing JSON recovery mirrors")
	}
	beforeJobs, beforeImports, err := sqlitePrimaryDecodeImages(jobImage.Before, importImage.Before)
	if err != nil {
		return err
	}
	if _, _, err := sqlitePrimaryDecodeImages(afterJobs, afterImports); err != nil {
		return err
	}
	journal := sqlitePrimaryTransaction{Version: sqlitePrimaryTransactionVersion, Jobs: jobImage, Imports: importImage}
	raw, err := encodeSQLitePrimaryTypedJournal(journal, beforeJobs, beforeImports, jobs, imports, limit)
	if err != nil {
		return err
	}
	// Recover only after the SQL transaction has closed, including a failure
	// during journal staging (rename may precede a directory fsync error).
	stagingAttempted := false
	defer func() {
		if err == nil || !stagingAttempted {
			return
		}
		if recoveryErr := recoverSQLitePrimaryCommitLocked(root); recoveryErr != nil {
			err = fmt.Errorf("%w; SQL primary transaction recovery failed (service must stop): %v", err, recoveryErr)
		}
	}()
	stage := func() error {
		stagingAttempted = true
		if err := writeSyncedMetadata(root, sqlitePrimaryTransactionName, raw); err != nil {
			return err
		}
		if hooks != nil && hooks.checkpoint != nil {
			return hooks.checkpoint("prepared")
		}
		return nil
	}
	var extra func(context.Context, *sql.Tx) error
	if hooks != nil {
		extra = hooks.reconcileExtra
	}
	if err := m.applyCheckedPrimaryDelta(context.Background(), beforeJobs, beforeImports, jobs, imports, stage, extra); err != nil {
		return err
	}
	if hooks != nil && hooks.checkpoint != nil {
		if err := hooks.checkpoint("sql-committed"); err != nil {
			return err
		}
	}
	// SQL has now committed. Never undo its new ACLs merely because a JSON
	// mirror write or cleanup failed. The durable primary journal will
	// complete the mirror before any subsequent service startup.
	if err := publishSQLitePrimaryMirrors(root, journal, hooks); err != nil {
		return err
	}
	if hooks != nil && hooks.checkpoint != nil {
		if err := hooks.checkpoint("json-committed"); err != nil {
			return err
		}
	}
	if err := m.verifyJSONParity(context.Background(), jobs, imports); err != nil {
		return fmt.Errorf("SQL changed unexpectedly after commit: %w", err)
	}
	if err := removeSyncedMetadata(root, sqlitePrimaryTransactionName); err != nil {
		return err
	}
	return nil
}

// publishSQLitePrimaryMirrors runs only after a durable SQL commit with a
// durable SQL intent. The intent itself protects both atomic renames, so a
// second full JSON journal is unnecessary. Validate BOTH files before writing
// either and skip an exact after-image, including an unchanged mirror.
func publishSQLitePrimaryMirrors(root string, journal sqlitePrimaryTransaction, hooks *sqlitePrimaryCommitHooks) error {
	if _, err := os.Lstat(filepath.Join(root, metadataJournalName)); err == nil {
		return errors.New("unexpected legacy JSON journal during SQL mirror publication")
	} else if !os.IsNotExist(err) {
		return err
	}
	targets := []struct {
		name    string
		image   metadataImage
		phase   string
		current bool
	}{
		{name: "jobs.json", image: journal.Jobs, phase: "jobs-mirror"},
		{name: "imports.json", image: journal.Imports, phase: "imports-mirror"},
	}
	for i := range targets {
		target := &targets[i]
		file, err := openArchiveRegular(root, target.name)
		if err != nil {
			return err
		}
		// Compare exact bytes in bounded chunks; neither mirror needs
		// another full-sized buffer just to establish its generation.
		current, err := matchSQLiteMirror(file, target.image.Before, target.image.After)
		closeErr := file.Close()
		if err != nil {
			return fmt.Errorf("SQL mirror %s: %w", target.name, err)
		}
		if closeErr != nil {
			return closeErr
		}
		target.current = current
	}
	for _, target := range targets {
		if !target.current {
			if err := writeSyncedMetadata(root, target.name, target.image.After); err != nil {
				return err
			}
		}
		if hooks != nil && hooks.checkpoint != nil {
			if err := hooks.checkpoint(target.phase); err != nil {
				return err
			}
		}
	}
	// A previous rename may have succeeded just before a directory fsync
	// failure. Even when both images already match, sync before cleanup.
	return syncMetadataDirectory(root)
}

// encodeSQLitePrimaryJournal checks the serialized size, including base64
// expansion of both metadata images. Reject before persisting intent or SQL COMMIT.
func encodeSQLitePrimaryJournal(journal sqlitePrimaryTransaction, limit int) ([]byte, error) {
	raw, err := json.Marshal(journal)
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("SQL primary journal exceeds %d bytes; transaction not committed", limit)
	}
	return raw, nil
}

// readSQLitePrimaryJournal pins the inspected regular file and bounds the read
// even if its size changes after Lstat. A replaced symlink or FIFO cannot be read.
func readSQLitePrimaryJournal(path string, expected os.FileInfo) ([]byte, error) {
	if !expected.Mode().IsRegular() || expected.Mode().Perm()&0077 != 0 ||
		expected.Size() <= 0 || expected.Size() > sqlitePrimaryJournalMaxBytes {
		return nil, errors.New("unsafe or oversized SQL primary transaction journal")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(expected, info) || !info.Mode().IsRegular() ||
		info.Mode().Perm()&0077 != 0 || info.Size() != expected.Size() {
		return nil, errors.New("SQL primary journal replaced or changed")
	}
	raw, err := io.ReadAll(io.LimitReader(file, sqlitePrimaryJournalMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > sqlitePrimaryJournalMaxBytes || int64(len(raw)) != info.Size() {
		return nil, errors.New("SQL primary journal size changed during read")
	}
	return raw, nil
}

// recoverSQLitePrimaryCommitLocked is called before any account migration,
// authentication, HTTP request or background worker can access archive ACLs.
// Caller owns the directory OS lock; a corrupt/unknown marker fails closed.
func recoverSQLitePrimaryCommitLocked(root string) error {
	path := filepath.Join(root, sqlitePrimaryTransactionName)
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	before, err := readSQLitePrimaryJournal(path, info)
	if err != nil {
		return err
	}
	journal, err := decodeSQLitePrimaryJournal(root, before)
	if err != nil {
		return fmt.Errorf("malformed SQL primary journal: %w", err)
	}
	if journal.Version != sqlitePrimaryTransactionVersion {
		return errors.New("unsupported SQL primary journal version")
	}
	if err := validateMetadataImage(journal.Jobs); err != nil {
		return err
	}
	if err := validateMetadataImage(journal.Imports); err != nil {
		return err
	}
	if !journal.Jobs.Existed || !journal.Imports.Existed {
		return errors.New("SQL primary journal is missing its prior JSON recovery point")
	}
	beforeJobs, beforeImports, err := sqlitePrimaryDecodeImages(journal.Jobs.Before, journal.Imports.Before)
	if err != nil {
		return err
	}
	afterJobs, afterImports, err := sqlitePrimaryDecodeImages(journal.Jobs.After, journal.Imports.After)
	if err != nil {
		return err
	}
	// Before changing ANY file, prove both current targets are exactly
	// one of the two journal images (or interrupted JSON pair images).
	for _, target := range []struct {
		name string
		img  metadataImage
	}{
		{"jobs.json", journal.Jobs},
		{"imports.json", journal.Imports},
	} {
		if err := validateMetadataRecoveryTarget(root, target.name, target.img); err != nil {
			return err
		}
	}
	// SQLite itself recovers a committed WAL after SIGKILL. An offline
	// no-WAL preflight here would incorrectly reject an ordinary crash.
	// The directory OS lock excludes cooperating concurrent writers.
	db, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		return err
	}
	defer db.close()
	afterMatches := db.verifyJSONParity(context.Background(), afterJobs, afterImports) == nil
	switch {
	case afterMatches:
		// A successful SQL commit, even one lost between the COMMIT return
		// and the next line of Go, MUST roll forward the JSON mirror.
		// Older binaries may have left their nested JSON journal.
		if err := recoverMetadataPair(root); err != nil {
			return err
		}
		if err := publishSQLitePrimaryMirrors(root, journal, nil); err != nil {
			return err
		}
	case db.verifyJSONParity(context.Background(), beforeJobs, beforeImports) == nil:
		// SQL did not commit. JSON must not have advanced; a mismatch
		// indicates corruption or an uncooperative external writer.
		for _, image := range []struct {
			name string
			img  metadataImage
		}{
			{"jobs.json", journal.Jobs},
			{"imports.json", journal.Imports},
		} {
			actual, err := os.ReadFile(filepath.Join(root, image.name))
			if err != nil || !bytes.Equal(actual, image.img.Before) {
				return fmt.Errorf("SQL stayed before but %s unexpectedly changed", image.name)
			}
		}
	default:
		return errors.New("SQL primary recovery found neither journal before nor after state; refusing to overwrite ACLs")
	}
	// A swapped journal must not be unlinked based on stale validation.
	now, err := readSQLitePrimaryJournal(path, info)
	if err != nil || !bytes.Equal(now, before) {
		return errors.New("SQL primary journal was changed during recovery")
	}
	if err := removeSyncedMetadata(root, sqlitePrimaryTransactionName); err != nil {
		return fmt.Errorf("SQL primary journal cleanup failed: %w", err)
	}
	return nil
}

func sqlitePrimaryDecodeImages(jobsRaw, importsRaw []byte) (map[string]Job, map[string]AuthorImport, error) {
	var jobs map[string]Job
	if err := json.Unmarshal(jobsRaw, &jobs); err != nil || jobs == nil {
		return nil, nil, errors.New("invalid SQL primary jobs image")
	}
	for id, job := range jobs {
		if id != job.ID {
			return nil, nil, errors.New("SQL primary jobs image has inconsistent ID")
		}
		if err := validSQLiteArchive(job); err != nil {
			return nil, nil, fmt.Errorf("invalid SQL primary job %s: %w", id, err)
		}
	}
	var imports map[string]AuthorImport
	if err := json.Unmarshal(importsRaw, &imports); err != nil || imports == nil {
		return nil, nil, errors.New("invalid SQL primary imports image")
	}
	for id, imp := range imports {
		if id != imp.ID {
			return nil, nil, errors.New("SQL primary imports image has inconsistent ID")
		}
		if err := validSQLiteImport(imp); err != nil {
			return nil, nil, fmt.Errorf("invalid SQL primary import %s: %w", id, err)
		}
	}
	return jobs, imports, nil
}

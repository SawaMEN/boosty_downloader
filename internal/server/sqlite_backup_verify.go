package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// VerifySQLiteMetadataBackup verifies a previously exported, PRIVATE metadata
// rollback snapshot without changing files or depending on live media paths.
// It verifies the manifest AND the SQL payloads/grants against backed-up JSON.
// It does not verify archive media, which is not in a metadata-only snapshot.
func VerifySQLiteMetadataBackup(dir string) error {
	if dir == "" {
		return errors.New("metadata backup directory is required")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("unsafe metadata backup directory permissions or type")
	}
	raw, err := backupReadPrivateFile(dir, "backup-manifest.json")
	if err != nil {
		return err
	}
	var manifest metadataBackupManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	if manifest.Version != 1 || manifest.CapturedAt.IsZero() ||
		manifest.CapturedAt.After(time.Now().UTC().Add(5*time.Minute)) {
		return errors.New("unsupported or invalid backup manifest timestamp/version")
	}
	allowed := map[string]bool{
		"jobs.json": true, "imports.json": true,
		"authors.json": true, "accounts.json": true,
		"metadata.db": true,
	}
	required := map[string]bool{"jobs.json": true, "imports.json": true, "metadata.db": true}
	seen := make(map[string]bool)
	for _, entry := range manifest.Files {
		if !allowed[entry.Name] || seen[entry.Name] ||
			entry.Size < 0 || len(entry.SHA256) != 64 {
			return fmt.Errorf("unexpected, repeated or invalid backup file entry: %q", entry.Name)
		}
		seen[entry.Name] = true
		delete(required, entry.Name)
		digest, size, err := backupFileDigest(dir, entry.Name)
		if err != nil {
			return err
		}
		if size != entry.Size || digest != entry.SHA256 {
			return fmt.Errorf("backup SHA-256 mismatch for %s", entry.Name)
		}
		if filepath.Ext(entry.Name) == ".json" {
			raw, err := backupReadPrivateFile(dir, entry.Name)
			if err != nil || !json.Valid(raw) {
				return fmt.Errorf("backup contains invalid JSON: %s", entry.Name)
			}
		}
	}
	if len(required) != 0 {
		return errors.New("backup manifest is missing jobs, imports or SQLite")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "backup-manifest.json" && !seen[entry.Name()] {
			return fmt.Errorf("unrecognized file in metadata backup: %s", entry.Name())
		}
	}
	jobs, err := sqliteShadowReadJobs(filepath.Join(dir, "jobs.json"))
	if err != nil {
		return err
	}
	imports, err := sqliteShadowReadImports(filepath.Join(dir, "imports.json"))
	if err != nil {
		return err
	}
	// This read-only SQL connection cannot create an empty database, change
	// journal_mode or repair a corrupted backup silently.
	dbPath := filepath.Join(dir, "metadata.db")
	db, err := openSQLiteReadOnly(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := sqliteAuditDatabaseIntegrity(db); err != nil {
		return err
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil) // connection itself is immutable + read-only
	if err != nil {
		return err
	}
	defer tx.Rollback()
	storedJobs, err := sqliteShadowSQLJobs(ctx, tx)
	if err != nil {
		return err
	}
	storedImports, err := sqliteShadowSQLImports(ctx, tx)
	if err != nil {
		return err
	}
	if len(jobs) != len(storedJobs) || len(imports) != len(storedImports) {
		return errors.New("backup JSON and SQL metadata row counts differ")
	}
	for id, want := range jobs {
		if !sqliteShadowEqual(want, storedJobs[id]) {
			return fmt.Errorf("backup archive payload differs from SQLite: %s", id)
		}
	}
	for id, want := range imports {
		if !sqliteShadowEqual(want, storedImports[id]) {
			return fmt.Errorf("backup import payload differs from SQLite: %s", id)
		}
	}
	return nil
}

func backupReadPrivateFile(dir, name string) ([]byte, error) {
	info, err := os.Lstat(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("unsafe backup metadata file permissions or type: %s", name)
	}
	if name == "backup-manifest.json" && info.Size() > 1<<20 {
		return nil, errors.New("backup manifest exceeds 1 MiB")
	}
	// Do not follow a replaced symlink if the directory was raced.
	file, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openInfo) {
		return nil, errors.New("backup entry changed while opening")
	}
	return io.ReadAll(file)
}

func backupFileDigest(dir, name string) (string, int64, error) {
	path := filepath.Join(dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", 0, fmt.Errorf("unsafe backup file: %s", name)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", 0, errors.New("backup file changed before reading")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	after, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, after) ||
		size != opened.Size() || size != after.Size() ||
		!opened.ModTime().Equal(after.ModTime()) {
		return "", 0, errors.New("backup file changed while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), size, nil
}

func openSQLiteReadOnly(path string) (*sql.DB, error) {
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("invalid SQLite backup file: %w", err)
	}
	// Append a URL escaped path so spaces and special characters cannot
	// change query options. Read-only connection never creates a new file.
	uri := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1&_pragma=query_only(1)"}).String()
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	var versionCount, version int
	if err := db.QueryRow("SELECT COUNT(*),COALESCE(MAX(migration_id),0) FROM schema_migrations").Scan(
		&versionCount, &version); err != nil || versionCount != 1 || version != 1 {
		db.Close()
		return nil, errors.New("unexpected backup SQLite schema version")
	}
	return db, nil
}

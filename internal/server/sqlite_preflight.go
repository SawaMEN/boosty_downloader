package server

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"

	_ "modernc.org/sqlite"
)

// verifySQLiteExport performs an explicitly opted-in read-only comparison of an
// offline migration against the authoritative JSON files. It never changes
// metadata, opens the exported database in mode=ro and never grants access.
func verifySQLiteExport(root string) error {
	dbPath := filepath.Join(root, "metadata.db")
	info, err := os.Lstat(dbPath)
	if err != nil {
		return fmt.Errorf("SQLite export missing: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("SQLite export must be a regular, non-symlink file")
	}
	jobsFile, jobsBytes, err := sqliteSourceJSON(filepath.Join(root, "jobs.json"))
	if err != nil {
		return err
	}
	importsFile := filepath.Join(root, "imports.json")
	_, importsBytes, err := sqliteSourceJSON(importsFile)
	if os.IsNotExist(err) {
		importsBytes = []byte("{}")
	} else if err != nil {
		return err
	}
	var jobs map[string]json.RawMessage
	if err := json.Unmarshal(jobsBytes, &jobs); err != nil || jobs == nil {
		return errors.New("jobs.json is invalid")
	}
	var imports map[string]json.RawMessage
	if err := json.Unmarshal(importsBytes, &imports); err != nil || imports == nil {
		return errors.New("imports.json is invalid")
	}
	// Go's URL encoder makes spaces, non-ASCII data directory names and '#'
	// safe. Query-only mode is enforced by the SQLite driver, not convention.
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return err
	}
	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&_pragma=query_only(1)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return fmt.Errorf("SQLite open failed: %w", err)
	}
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("SQLite integrity check failed: %s: %w", integrity, err)
	}
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	if rows.Next() {
		rows.Close()
		return errors.New("SQLite foreign key verification failed")
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var migrations int
	if err := db.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE migration_id=1").Scan(&migrations); err != nil || migrations != 1 {
		return fmt.Errorf("unknown SQLite migration version: %w", err)
	}
	seen := make(map[string]bool, len(jobs))
	grants := make(map[string]map[string]bool)
	rows, err = db.Query(`SELECT a.archive_id,a.owner_id,p.author_slug,p.remote_post_id,
		a.job_state,a.shared_for_all,a.payload_json
		FROM owner_archives a JOIN source_posts p ON a.post_key=p.post_key`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, owner, author, postID, state, payload string
		var everyone int
		if err := rows.Scan(&id, &owner, &author, &postID, &state, &everyone, &payload); err != nil {
			rows.Close()
			return err
		}
		source, ok := jobs[id]
		if !ok || seen[id] {
			rows.Close()
			return fmt.Errorf("unexpected or duplicate SQLite archive %q", id)
		}
		if !sqliteSameJSON(source, []byte(payload)) {
			rows.Close()
			return fmt.Errorf("SQLite job payload changed: %q", id)
		}
		var job struct {
			ID           string   `json:"id"`
			OwnerID      string   `json:"ownerId"`
			Author       string   `json:"author"`
			PostID       string   `json:"postId"`
			State        string   `json:"state"`
			SharedWith   []string `json:"sharedWith"`
			SharedForAll bool     `json:"sharedForAll"`
		}
		if json.Unmarshal(source, &job) != nil ||
			job.ID != id || job.OwnerID != owner || job.Author != author ||
			job.PostID != postID || job.State != state ||
			job.SharedForAll != (everyone != 0) {
			rows.Close()
			return fmt.Errorf("SQLite archive identity mismatch: %q", id)
		}
		grants[id] = make(map[string]bool)
		for _, member := range job.SharedWith {
			if !safeArchivePart(member) || grants[id][member] {
				rows.Close()
				return fmt.Errorf("invalid JSON access grant: %q", id)
			}
			grants[id][member] = true
		}
		seen[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(seen) != len(jobs) {
		return fmt.Errorf("SQLite archive count mismatch: %d / %d", len(seen), len(jobs))
	}
	rows, err = db.Query("SELECT archive_id,grantee_owner_id FROM archive_grants")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, member string
		if err := rows.Scan(&id, &member); err != nil {
			rows.Close()
			return err
		}
		if !grants[id][member] {
			rows.Close()
			return fmt.Errorf("unexpected SQLite access grant: %s", id)
		}
		delete(grants[id], member)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for id, pending := range grants {
		if len(pending) != 0 {
			return fmt.Errorf("missing SQLite access grants: %s", id)
		}
	}
	seen = make(map[string]bool, len(imports))
	rows, err = db.Query("SELECT import_id,owner_id,author_slug,job_state,payload_json FROM author_imports")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, owner, author, state, payload string
		if err := rows.Scan(&id, &owner, &author, &state, &payload); err != nil {
			rows.Close()
			return err
		}
		source, ok := imports[id]
		if !ok || seen[id] || !sqliteSameJSON(source, []byte(payload)) {
			rows.Close()
			return fmt.Errorf("SQLite import mismatch: %q", id)
		}
		var imp struct {
			ID      string `json:"id"`
			OwnerID string `json:"ownerId"`
			Author  string `json:"author"`
			State   string `json:"state"`
		}
		if json.Unmarshal(source, &imp) != nil || imp.ID != id ||
			imp.OwnerID != owner || imp.Author != author || imp.State != state {
			rows.Close()
			return fmt.Errorf("SQLite import identity mismatch: %q", id)
		}
		seen[id] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(seen) != len(imports) {
		return fmt.Errorf("SQLite import count mismatch: %d / %d", len(seen), len(imports))
	}
	// A writer racing with verification is a hard failure, not a reason to
	// trust stale metadata. No SQL database state is activated on failure.
	_, afterJobs, err := sqliteSourceJSON(jobsFile)
	if err != nil || sha256.Sum256(afterJobs) != sha256.Sum256(jobsBytes) {
		return errors.New("jobs.json changed during SQLite verification")
	}
	_, afterImports, err := sqliteSourceJSON(importsFile)
	if os.IsNotExist(err) {
		afterImports = []byte("{}")
	} else if err != nil {
		return err
	}
	if sha256.Sum256(afterImports) != sha256.Sum256(importsBytes) {
		return errors.New("imports.json changed during SQLite verification")
	}
	return nil
}

func sqliteSourceJSON(path string) (string, []byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return path, nil, err
	}
	if !info.Mode().IsRegular() {
		return path, nil, fmt.Errorf("metadata is not a regular file: %s", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	return path, data, err
}

func sqliteSameJSON(a, b []byte) bool {
	var left, right any
	return json.Unmarshal(a, &left) == nil &&
		json.Unmarshal(b, &right) == nil &&
		reflect.DeepEqual(left, right)
}

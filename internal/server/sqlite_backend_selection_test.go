package server

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPinJSONBackendIsCreateOnlyPrivateAndDoesNotMutateData(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	oldJSON, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	oldSQL, err := os.ReadFile(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := PinJSONBackendSelection(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, sqliteBackendSelectionName)
	mode, err := os.Lstat(path)
	if err != nil || mode.Mode().Perm() != 0600 || !mode.Mode().IsRegular() {
		t.Fatalf("backend selector is not private: %v %v", mode, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := PinJSONBackendSelection(root); err != nil {
		t.Fatal("idempotent JSON pin rejected:", err)
	}
	again, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("second pin rewrote the existing backend selector")
	}
	active, err := readSQLiteBackendSelection(root)
	if err != nil || active != "json" {
		t.Fatalf("durable JSON marker was not accepted: %q, %v", active, err)
	}
	newJSON, err := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil || !bytes.Equal(oldJSON, newJSON) {
		t.Fatal("pin operation rewrote archive JSON")
	}
	newSQL, err := os.ReadFile(filepath.Join(root, "metadata.db"))
	if err != nil || !bytes.Equal(oldSQL, newSQL) {
		t.Fatal("pin operation rewrote SQLite database")
	}
	if store, err := newStorage(root, testKey(t)); err != nil || store == nil {
		t.Fatalf("JSON backend could not restart with durable pin: %v", err)
	}
}

func TestPinJSONBackendFailsWhenDataLockedOrCutoverPending(t *testing.T) {
	for _, scenario := range []string{"locked", "metadata-journal", "sql-primary-journal", "cutover-intent", "wal"} {
		t.Run(scenario, func(t *testing.T) {
			root, _ := sqliteCanaryFixture(t)
			if scenario == "locked" {
				lock, err := lockDataDirectory(root)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			} else {
				var pending string
				switch scenario {
				case "metadata-journal":
					pending = metadataJournalName
				case "sql-primary-journal":
					pending = sqlitePrimaryTransactionName
				case "cutover-intent":
					pending = sqliteCutoverIntentName
				default:
					pending = "metadata.db-wal"
				}
				if err := os.WriteFile(filepath.Join(root, pending), []byte("not a committed record"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := PinJSONBackendSelection(root); err == nil {
				t.Fatal("JSON backend pinned while data directory was busy or unsafe")
			}
			if _, err := os.Lstat(filepath.Join(root, sqliteBackendSelectionName)); !os.IsNotExist(err) {
				t.Fatal("failed backend selection published a marker")
			}
		})
	}
}

func TestSQLiteActiveBackendMarkerBlocksLegacyJSONStartup(t *testing.T) {
	for _, scenario := range []string{"sqlite", "unknown", "malformed", "symlink", "world-readable"} {
		t.Run(scenario, func(t *testing.T) {
			root, _ := sqliteCanaryFixture(t)
			path := filepath.Join(root, sqliteBackendSelectionName)
			selector := []byte(`{"version":1,"active":"json"}`)
			switch scenario {
			case "sqlite":
				selector = []byte(`{"version":1,"active":"sqlite"}`)
			case "unknown":
				selector = []byte(`{"version":3,"active":"json"}`)
			case "malformed":
				selector = []byte(`{"version":1,"active":"json"}{"extra":true}`)
			}
			if scenario == "symlink" {
				if err := os.Symlink(filepath.Join(root, "jobs.json"), path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, selector, 0600); err != nil {
					t.Fatal(err)
				}
				if scenario == "world-readable" {
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			old, err := os.ReadFile(filepath.Join(root, "jobs.json"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := newStorage(root, testKey(t)); err == nil {
				t.Fatal("legacy JSON Storage accepted unsupported or corrupted backend selector")
			}
			app, err := New(Config{
				DataDir: root, AdminUser: "admin", AdminPassword: "strong-password-123",
				SecretKey: testKey(t),
			})
			if err == nil {
				app.Close()
				t.Fatal("HTTP server started with unsupported or unsafe backend selector")
			}
			after, err := os.ReadFile(filepath.Join(root, "jobs.json"))
			if err != nil || !bytes.Equal(old, after) {
				t.Fatal("unsafe backend selector permitted JSON rewriting")
			}
		})
	}
}

func TestPinJSONBackendRefusesMissingSnapshotAndExistingForeignSelector(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	if err := os.Remove(filepath.Join(root, "imports.json")); err != nil {
		t.Fatal(err)
	}
	if err := PinJSONBackendSelection(root); err == nil {
		t.Fatal("missing import snapshot was accepted as safe JSON backend")
	}
	if _, err := os.Lstat(filepath.Join(root, sqliteBackendSelectionName)); !os.IsNotExist(err) {
		t.Fatal("failed selector pin wrote marker")
	}
	root, _ = sqliteCanaryFixture(t)
	path := filepath.Join(root, sqliteBackendSelectionName)
	if err := os.WriteFile(path, []byte(`{"version":1,"active":"sqlite"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := PinJSONBackendSelection(root); err == nil || !strings.Contains(err.Error(), "cannot replace") {
		t.Fatalf("pin silently switched SQLite back to JSON: %v", err)
	}
}

func TestUnsupportedSQLMarkerBlocksCommittedLegacyJournalBeforeMutation(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	jobsPath := filepath.Join(root, "jobs.json")
	original, err := os.ReadFile(jobsPath)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := sqliteShadowReadJobs(jobsPath)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("1", 24)
	j := jobs[id]
	j.Title = "must never roll forward"
	j.UpdatedAt = j.UpdatedAt.Add(time.Minute)
	jobs[id] = j
	newJobs, err := json.Marshal(jobs)
	if err != nil {
		t.Fatal(err)
	}
	origImports, err := os.ReadFile(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	jobImage, err := checkedMetadataImage(root, "jobs.json", newJobs)
	if err != nil {
		t.Fatal(err)
	}
	importImage, err := checkedMetadataImage(root, "imports.json", origImports)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := json.Marshal(metadataTransaction{
		Version: metadataJournalVersion, Phase: "committed",
		Jobs: jobImage, Imports: importImage,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, metadataJournalName), journal, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, sqliteBackendSelectionName),
		[]byte(`{"version":1,"active":"sqlite"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, testKey(t)); err == nil {
		t.Fatal("SQL-selected backend recovered a legacy JSON journal before rejecting startup")
	}
	if app, err := New(Config{
		DataDir: root, AdminUser: "admin", AdminPassword: "strong-password-123",
		SecretKey: testKey(t),
	}); err == nil {
		app.Close()
		t.Fatal("server recovered JSON journal while SQL was selected")
	}
	after, err := os.ReadFile(jobsPath)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("SQL-selected deployment's JSON was modified before backend validation")
	}
	remaining, err := os.ReadFile(filepath.Join(root, metadataJournalName))
	if err != nil || !bytes.Equal(journal, remaining) {
		t.Fatal("legacy JSON transaction journal was unexpectedly consumed")
	}
}

func TestPinExistingJSONBackendStillChecksPendingJournal(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	if err := PinJSONBackendSelection(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, sqliteBackendSelectionName)
	marker, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, sqlitePrimaryTransactionName), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := PinJSONBackendSelection(root); err == nil {
		t.Fatal("repeated backend pin ignored an unfinished SQL-first transaction")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(marker, got) {
		t.Fatal("failed repeat changed the original backend selector")
	}
}

func TestPinJSONBackendRefusesSQLOnlyGrantUpdates(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("1", 24)
	before, err := meta.job(context.Background(), "first", id)
	if err != nil {
		t.Fatal(err)
	}
	after := before
	after.SharedWith = []string{"new-sql-member"}
	after.UpdatedAt = before.UpdatedAt.Add(time.Minute)
	if err := meta.apply(context.Background(),
		[]sqliteArchiveChange{{Before: &before, After: after}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := meta.close(); err != nil {
		t.Fatal(err)
	}
	if err := PinJSONBackendSelection(root); err == nil {
		t.Fatal("JSON backend was pinned despite newer SQL-only grants")
	}
	if _, err := os.Lstat(filepath.Join(root, sqliteBackendSelectionName)); !os.IsNotExist(err) {
		t.Fatal("unsafe JSON selection was published")
	}
}

func TestPinJSONBackendSupportsLegacyDataWithoutSQLite(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	if err := os.Remove(filepath.Join(root, "metadata.db")); err != nil {
		t.Fatal(err)
	}
	if err := PinJSONBackendSelection(root); err != nil {
		t.Fatal("JSON-only legacy deployment should be supported:", err)
	}
	got, err := readSQLiteBackendSelection(root)
	if err != nil || got != "json" {
		t.Fatalf("legacy JSON marker not durable: %q %v", got, err)
	}
}

func TestPinExistingJSONBackendRejectsForgedSQLIndexEvenWhenMetadataMatches(t *testing.T) {
	root, _ := sqliteCanaryFixture(t)
	if err := PinJSONBackendSelection(root); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(root, sqliteBackendSelectionName)
	pinned, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := meta.db.Exec("UPDATE owner_archives SET title='forged-sql-title' WHERE archive_id=?",
		strings.Repeat("1", 24)); err != nil {
		t.Fatal(err)
	}
	if err := meta.close(); err != nil {
		t.Fatal(err)
	}
	if err := PinJSONBackendSelection(root); err == nil {
		t.Fatal("JSON backend pin ignored forged SQL indexed title")
	}
	after, err := os.ReadFile(markerPath)
	if err != nil || !bytes.Equal(pinned, after) {
		t.Fatal("failed pin modified a previously committed backend selector")
	}
}

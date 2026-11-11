package server

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sqliteReindexSetSubscription(t *testing.T, root, revisionID, level string) {
	t.Helper()
	manifestPath := filepath.Join(root, "revisions", "first", "creator", "post-1", revisionID, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest ArchiveManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Subscription = &CapturedSubscription{
		Level: level, LevelID: "paid-tier", Paid: true, Paused: false,
		ObservedAt: manifest.CapturedAt, Source: "boosty_subscriptions",
	}
	raw, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func sqliteReindexObservationRecord(t *testing.T, root, revisionID string) (int64, string, int) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id int64
	var owner string
	if err := db.QueryRow(`SELECT r.subscription_observation_id,o.owner_id
		FROM archive_revisions r JOIN subscription_observations o
		ON o.observation_id=r.subscription_observation_id
		WHERE r.revision_id=?`, revisionID).Scan(&id, &owner); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM subscription_observations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return id, owner, count
}

func TestSQLiteOfflineReindexPreservesSubscriptionAndOwnerAcrossRepeatedRuns(t *testing.T) {
	root, backup, _, historyID, media := sqliteReindexFixture(t)
	newID := strings.Repeat("e", 24)
	sqliteReindexCopyHistory(t, root, historyID, newID, media)
	sqliteReindexSetSubscription(t, root, newID, "Gold")
	if err := ReindexSQLiteRevisionInventory(root, backup); err != nil {
		t.Fatal(err)
	}
	original, owner, observations := sqliteReindexObservationRecord(t, root, newID)
	if owner != "first" || observations != 1 {
		t.Fatalf("subscription was assigned outside owner scope: %s %d", owner, observations)
	}
	freshBackup := filepath.Join(t.TempDir(), "after-first-reindex")
	if err := PrepareSQLiteBackup(root, freshBackup); err != nil {
		t.Fatal(err)
	}
	if err := ReindexSQLiteRevisionInventory(root, freshBackup); err != nil {
		t.Fatal(err)
	}
	again, owner, count := sqliteReindexObservationRecord(t, root, newID)
	if again != original || owner != "first" || count != observations {
		t.Fatalf("reindex duplicated or reassigned subscription: %d -> %d, count=%d", original, again, count)
	}
	if err := VerifySQLiteSnapshotInventory(root); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteOfflineReindexRejectsHistoricalSubscriptionTampering(t *testing.T) {
	root, backup, _, historyID, media := sqliteReindexFixture(t)
	newID := strings.Repeat("f", 24)
	sqliteReindexCopyHistory(t, root, historyID, newID, media)
	sqliteReindexSetSubscription(t, root, newID, "Gold")
	if err := ReindexSQLiteRevisionInventory(root, backup); err != nil {
		t.Fatal(err)
	}
	original, _, _ := sqliteReindexObservationRecord(t, root, newID)
	freshBackup := filepath.Join(t.TempDir(), "locked-history")
	if err := PrepareSQLiteBackup(root, freshBackup); err != nil {
		t.Fatal(err)
	}
	sqliteReindexSetSubscription(t, root, newID, "Forged")
	if err := ReindexSQLiteRevisionInventory(root, freshBackup); err == nil {
		t.Fatal("historical subscription was changed without archive restoration")
	}
	after, owner, count := sqliteReindexObservationRecord(t, root, newID)
	if after != original || owner != "first" || count != 1 {
		t.Fatal("failed historical update modified SQL subscription data")
	}
}

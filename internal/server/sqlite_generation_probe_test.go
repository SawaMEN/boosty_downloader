package server

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSQLiteGenerationProbeConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "generation.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(2)
	first, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.ExecContext(ctx, "CREATE TABLE probe(id INTEGER PRIMARY KEY, value TEXT)"); err != nil {
		t.Fatal(err)
	}
	second, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	sample := func() sqliteGenerationProbe {
		t.Helper()
		probe, err := sqliteReadGenerationProbe(ctx, first)
		if err != nil {
			t.Fatal(err)
		}
		return probe
	}
	initial := sample()
	if again := sample(); again != initial {
		t.Fatal("read-only probe changed generation")
	}
	if _, err := first.ExecContext(ctx, "INSERT INTO probe VALUES(1,'local')"); err != nil {
		t.Fatal(err)
	}
	local := sample()
	if local.TotalChanges <= initial.TotalChanges || local.DataVersion != initial.DataVersion {
		t.Fatalf("local write should change total_changes but not data_version: %+v -> %+v", initial, local)
	}
	if _, err := second.ExecContext(ctx, "UPDATE probe SET value='external' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	external := sample()
	if external.DataVersion == local.DataVersion || external.TotalChanges != local.TotalChanges {
		t.Fatalf("external write should change data_version but not total_changes: %+v -> %+v", local, external)
	}
	tx, err := first.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE probe SET value='rollback' WHERE id=1"); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	rolledBack := sample()
	if rolledBack.TotalChanges <= external.TotalChanges {
		t.Fatal("rollback must conservatively invalidate a local changes probe")
	}
	var value string
	if err := first.QueryRowContext(ctx, "SELECT value FROM probe WHERE id=1").Scan(&value); err != nil || value != "external" {
		t.Fatalf("rollback did not preserve value: %q, %v", value, err)
	}
	if _, err := second.ExecContext(ctx, "CREATE TABLE another(id INTEGER PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if changed := sample(); changed.SchemaVersion == rolledBack.SchemaVersion {
		t.Fatal("schema change was not detected")
	}
}

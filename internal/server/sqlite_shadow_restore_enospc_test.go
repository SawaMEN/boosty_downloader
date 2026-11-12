package server

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Run this test ONLY on a dedicated, size-limited Linux tmpfs in CI.
// Filling a shared or unbounded host filesystem would be unsafe. The explicit
// opt-in and filesystem checks prevent accidental execution on a VPS.
func TestSQLiteShadowRestorePhysicalENOSPC(t *testing.T) {
	mount := os.Getenv("BD_TEST_SQLITE_TMPFS")
	if mount == "" {
		t.Skip("requires a dedicated, size-limited tmpfs")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(mount, &fs); err != nil {
		t.Fatal(err)
	}
	const tmpfsMagic = 0x01021994
	if fs.Type != tmpfsMagic || int64(fs.Blocks)*fs.Bsize > 64<<20 {
		t.Fatal("refusing to fill a filesystem that is not a private <=64 MiB tmpfs")
	}
	if filepath.Clean(os.TempDir()) != filepath.Clean(mount) {
		t.Fatal("TMPDIR must point at the isolated tmpfs")
	}

	root, backup, _, _, _ := sqliteReindexFixture(t)
	dbPath := filepath.Join(root, "metadata.db")
	oldSQL := []byte("broken SQLite before real disk-full restore")
	if err := os.WriteFile(dbPath, oldSQL, 0600); err != nil {
		t.Fatal(err)
	}
	before := map[string][]byte{
		"jobs.json":    sqliteRestoreReadTestFile(t, filepath.Join(root, "jobs.json")),
		"imports.json": sqliteRestoreReadTestFile(t, filepath.Join(root, "imports.json")),
	}
	for _, name := range []string{"authors.json", "accounts.json"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err == nil {
			before[name] = raw
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}

	// The filler is OUTSIDE the service directory, so the source data and
	// verified metadata snapshot are not changed by the failure injection.
	fillerPath := filepath.Join(t.TempDir(), "fill-disk")
	filler, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	diskFull := false
	for i := 0; i < 80; i++ {
		_, err = filler.Write(chunk)
		if errors.Is(err, syscall.ENOSPC) {
			diskFull = true
			break
		}
		if err != nil {
			filler.Close()
			t.Fatal(err)
		}
	}
	if err := filler.Close(); err != nil {
		t.Fatal(err)
	}
	if !diskFull {
		t.Fatal("the isolated tmpfs did not actually report ENOSPC")
	}
	// Free one filesystem block: a temporary file can now be opened, but
	// copying the backed-up SQLite database still runs out of real blocks.
	// This exercises io.Copy rather than only os.CreateTemp failure.
	snapshotStat, err := os.Stat(filepath.Join(backup, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	if snapshotStat.Size() <= 8192 {
		t.Fatal("the fixture database is too small for the bounded ENOSPC test")
	}
	fillStat, err := os.Stat(fillerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(fillerPath, fillStat.Size()-4096); err != nil {
		t.Fatal(err)
	}

	err = RestoreSQLiteShadowFromBackup(root, backup)
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("real full filesystem must reject SQLite restore with ENOSPC, got: %v", err)
	}
	if !bytes.Equal(sqliteRestoreReadTestFile(t, dbPath), oldSQL) {
		t.Fatal("real ENOSPC replaced the original SQLite before verified copy")
	}
	sqliteRestoreCheckUnchangedJSON(t, root, before)
	sqliteRestoreCheckNoCurrentTemps(t, root)

	// Removing this filler is the equivalent of safely freeing the scratch
	// filesystem. Successful recovery must work without changing JSON/media.
	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSQLiteShadowFromBackup(root, backup); err != nil {
		t.Fatalf("recovery did not resume after freeing real disk space: %v", err)
	}
	sqliteRestoreCheckUnchangedJSON(t, root, before)
	sqliteRestoreCheckNoCurrentTemps(t, root)
	if err := VerifySQLiteCutoverPreflight(root, backup); err != nil {
		t.Fatalf("restored SQL does not match JSON/ACL/media after real ENOSPC: %v", err)
	}
}

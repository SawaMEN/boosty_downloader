package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Only the dedicated CI tmpfs can be exhausted. Fill after the real SQL
// commit, not by returning a synthetic error from a checkpoint hook.
func TestSQLPrimaryMirrorPhysicalENOSPC(t *testing.T) {
	mount := os.Getenv("BD_TEST_SQLITE_TMPFS")
	if mount == "" {
		t.Skip("requires a dedicated, size-limited tmpfs")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(mount, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != 0x01021994 || fs.Bsize <= 0 ||
		fs.Blocks > uint64(64<<20)/uint64(fs.Bsize) ||
		filepath.Clean(os.TempDir()) != filepath.Clean(mount) {
		t.Fatal("refusing to fill a filesystem that is not the isolated <=64 MiB tmpfs")
	}
	root, jobs, imports := sqlPrimaryJournalFixture(t)
	lock, err := lockDataDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if meta != nil {
			_ = meta.close()
		}
	}()
	before := map[string][]byte{}
	for _, name := range []string{"jobs.json", "imports.json"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = raw
	}
	fillerPath := filepath.Join(t.TempDir(), "fill-after-sql-commit")
	defer os.Remove(fillerPath)
	filled := false
	err = sqlitePrimaryCommitMetadata(root, meta, jobs, imports, &sqlitePrimaryCommitHooks{
		checkpoint: func(phase string) error {
			if phase != "sql-committed" {
				return nil
			}
			f, err := os.OpenFile(fillerPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				return err
			}
			defer f.Close()
			chunk := make([]byte, 1<<20)
			for i := 0; i < 65; i++ {
				if _, err := f.Write(chunk); errors.Is(err, syscall.ENOSPC) {
					filled = true
					// Do not return ENOSPC here: the actual mirror
					// publication must attempt a write on the full disk.
					return nil
				} else if err != nil {
					return err
				}
			}
			return fmt.Errorf("isolated tmpfs did not report ENOSPC")
		},
	})
	if !filled || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("expected real mirror ENOSPC after SQL COMMIT: filled=%v err=%v", filled, err)
	}
	if _, err := os.Lstat(filepath.Join(root, sqlitePrimaryTransactionName)); err != nil {
		t.Fatal("recovery intent lost on full disk", err)
	}
	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("full disk changed old mirror %s: %v", name, err)
		}
	}
	if err := os.Remove(fillerPath); err != nil {
		t.Fatal(err)
	}
	if err := meta.verifyJSONParity(context.Background(), jobs, imports); err != nil {
		t.Fatal("committed SQL ACL or import lost", err)
	}
	if err := meta.close(); err != nil {
		t.Fatal(err)
	}
	meta = nil
	if err := recoverMetadataPair(root); err != nil {
		t.Fatal(err)
	}
	if err := recoverSQLitePrimaryCommitLocked(root); err != nil {
		t.Fatal("recovery after freeing real space", err)
	}
	sqlPrimaryAssertImages(t, root, true)
	// Recovery is idempotent and must preserve the committed generation.
	if err := recoverSQLitePrimaryCommitLocked(root); err != nil {
		t.Fatal(err)
	}
	sqlPrimaryAssertImages(t, root, true)
}

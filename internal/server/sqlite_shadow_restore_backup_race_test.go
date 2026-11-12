package server

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The initial recovery verification must remain binding for the whole
// operation. A second hash of a changed source is not a new trusted baseline.
func TestSQLiteShadowRestoreRejectsBackupChangedDuringOperation(t *testing.T) {
	for _, phase := range []string{"audited", "staged"} {
		t.Run(phase, func(t *testing.T) {
			root, backup, _, _, _ := sqliteReindexFixture(t)
			dbPath := filepath.Join(root, "metadata.db")
			backupPath := filepath.Join(backup, "metadata.db")
			originalBackup := sqliteRestoreReadTestFile(t, backupPath)
			damagedLive := []byte("original corrupted SQL must survive rejected restore")
			if err := os.WriteFile(dbPath, damagedLive, 0600); err != nil {
				t.Fatal(err)
			}
			originalJobs := sqliteRestoreReadTestFile(t, filepath.Join(root, "jobs.json"))
			originalImports := sqliteRestoreReadTestFile(t, filepath.Join(root, "imports.json"))
			injected := false
			err := restoreSQLiteShadowWithHooks(root, backup, &restoreSQLiteShadowHooks{
				checkpoint: func(at string) error {
					if at != phase {
						return nil
					}
					injected = true
					// Keep the copied SQLite pages identical. The extra byte
					// changes the verified snapshot SHA-256 without needing to
					// corrupt JSON, media or mutate the live database.
					f, err := os.OpenFile(backupPath, os.O_APPEND|os.O_WRONLY, 0)
					if err != nil {
						return err
					}
					_, writeErr := f.Write([]byte{0})
					if closeErr := f.Close(); writeErr == nil {
						writeErr = closeErr
					}
					return writeErr
				},
			})
			if !injected {
				t.Fatal("test did not mutate the recovery point at the requested checkpoint")
			}
			if err == nil || !strings.Contains(err.Error(), "backup SQLite changed") {
				t.Fatalf("restore accepted a mutated verified recovery point: %v", err)
			}
			if got := sqliteRestoreReadTestFile(t, dbPath); !bytes.Equal(got, damagedLive) {
				t.Fatal("restore changed live SQL after detecting recovery point mutation")
			}
			sqliteRestoreCheckUnchangedJSON(t, root, map[string][]byte{
				"jobs.json":    originalJobs,
				"imports.json": originalImports,
			})
			sqliteRestoreCheckNoCurrentTemps(t, root)

			if err := os.WriteFile(backupPath, originalBackup, 0600); err != nil {
				t.Fatal(err)
			}
			if err := VerifySQLiteMetadataBackup(backup); err != nil {
				t.Fatalf("restored recovery snapshot did not validate: %v", err)
			}
			if err := RestoreSQLiteShadowFromBackup(root, backup); err != nil {
				t.Fatalf("recovery failed after restoring verified source bytes: %v", err)
			}
			if err := VerifySQLiteCutoverPreflight(root, backup); err != nil {
				t.Fatalf("retry did not recover SQL, JSON ACLs and real media: %v", err)
			}
		})
	}
}

package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// A writer can race the narrow gap between successful backup verification
// and the first SQL hash. Never accept its replacement as the new reference.
func TestSQLiteShadowRestoreRejectsMutationAfterVerification(t *testing.T) {
	for _, mutation := range []string{"sqlite", "manifest", "matching-sqlite-and-manifest"} {
		t.Run(mutation, func(t *testing.T) {
			root, backup, _, _, _ := sqliteReindexFixture(t)
			dbPath := filepath.Join(root, "metadata.db")
			backupSQLPath := filepath.Join(backup, "metadata.db")
			manifestPath := filepath.Join(backup, "backup-manifest.json")
			damagedLive := []byte("original damaged SQL must stay in place")
			if err := os.WriteFile(dbPath, damagedLive, 0600); err != nil {
				t.Fatal(err)
			}
			backupSQL := sqliteRestoreReadTestFile(t, backupSQLPath)
			originalManifest := sqliteRestoreReadTestFile(t, manifestPath)
			before := map[string][]byte{
				"jobs.json":    sqliteRestoreReadTestFile(t, filepath.Join(root, "jobs.json")),
				"imports.json": sqliteRestoreReadTestFile(t, filepath.Join(root, "imports.json")),
			}
			seen := false
			err := restoreSQLiteShadowWithHooks(root, backup, &restoreSQLiteShadowHooks{
				checkpoint: func(phase string) error {
					if phase != "verified" {
						return nil
					}
					seen = true
					if mutation != "manifest" {
						if err := os.WriteFile(backupSQLPath, append(backupSQL, 0), 0600); err != nil {
							return err
						}
					}
					if mutation == "manifest" {
						return os.WriteFile(manifestPath, append(originalManifest, '\n'), 0600)
					}
					if mutation != "matching-sqlite-and-manifest" {
						return nil
					}
					var manifest metadataBackupManifest
					if err := json.Unmarshal(originalManifest, &manifest); err != nil {
						return err
					}
					digest := sha256.Sum256(append(backupSQL, 0))
					for i := range manifest.Files {
						if manifest.Files[i].Name == "metadata.db" {
							manifest.Files[i].Size++
							manifest.Files[i].SHA256 = hex.EncodeToString(digest[:])
						}
					}
					newManifest, err := json.Marshal(manifest)
					if err != nil {
						return err
					}
					return os.WriteFile(manifestPath, newManifest, 0600)
				},
			})
			if !seen {
				t.Fatal("mutation checkpoint was not reached")
			}
			if err == nil || !strings.Contains(err.Error(), "verified backup") {
				t.Fatalf("accepted a mutated snapshot after successful verification: %v", err)
			}
			if got := sqliteRestoreReadTestFile(t, dbPath); !bytes.Equal(got, damagedLive) {
				t.Fatal("rejected snapshot mutation replaced the live SQLite file")
			}
			sqliteRestoreCheckUnchangedJSON(t, root, before)
			sqliteRestoreCheckNoCurrentTemps(t, root)

			if err := os.WriteFile(backupSQLPath, backupSQL, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, originalManifest, 0600); err != nil {
				t.Fatal(err)
			}
			if err := RestoreSQLiteShadowFromBackup(root, backup); err != nil {
				t.Fatalf("recovery was not retryable after restoring original backup: %v", err)
			}
			if err := VerifySQLiteCutoverPreflight(root, backup); err != nil {
				t.Fatalf("consistent recovery point failed after retry: %v", err)
			}
		})
	}
}

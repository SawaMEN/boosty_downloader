package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func metadataTxnFixture(t *testing.T) (root, key, jobID, importID string) {
	t.Helper()
	root, key = t.TempDir(), testKey(t)
	jobID, importID = strings.Repeat("1", 24), strings.Repeat("a", 24)
	at := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	jobs := map[string]*Job{
		jobID: {
			ID: jobID, OwnerID: "owner", Author: "creator", PostID: "post-1",
			Title: "Original", State: "done", CreatedAt: at, UpdatedAt: at,
		},
		strings.Repeat("2", 24): {
			ID: strings.Repeat("2", 24), OwnerID: "other", Author: "creator",
			PostID: "post-1", Title: "Unrelated", State: "failed",
			CreatedAt: at, UpdatedAt: at,
		},
	}
	imports := map[string]*AuthorImport{
		importID: {
			ID: importID, OwnerID: "owner", Author: "creator",
			State: "done", CreatedAt: at, UpdatedAt: at,
		},
	}
	jobsJSON, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	importsJSON, err := json.MarshalIndent(imports, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(root, "jobs.json"), jobsJSON); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(root, "imports.json"), importsJSON); err != nil {
		t.Fatal(err)
	}
	return root, key, jobID, importID
}

func makePendingMetadataTxn(t *testing.T, root, phase string) (oldJobs, oldImports, nextJobs, nextImports []byte) {
	t.Helper()
	var err error
	oldJobs, err = os.ReadFile(filepath.Join(root, "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	oldImports, err = os.ReadFile(filepath.Join(root, "imports.json"))
	if err != nil {
		t.Fatal(err)
	}
	var jobs map[string]Job
	var imports map[string]AuthorImport
	if err := json.Unmarshal(oldJobs, &jobs); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(oldImports, &imports); err != nil {
		t.Fatal(err)
	}
	for id, item := range jobs {
		item.AutoSync = true
		jobs[id] = item
	}
	for id, item := range imports {
		item.AutoSync = true
		imports[id] = item
	}
	nextJobs, _ = json.MarshalIndent(jobs, "", "  ")
	nextImports, _ = json.MarshalIndent(imports, "", "  ")
	j, err := checkedMetadataImage(root, "jobs.json", nextJobs)
	if err != nil {
		t.Fatal(err)
	}
	i, err := checkedMetadataImage(root, "imports.json", nextImports)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := encodeMetadataTransaction(metadataTransaction{
		Version: metadataJournalVersion, Phase: phase, Jobs: j, Imports: i,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSyncedMetadata(root, metadataJournalName, journal); err != nil {
		t.Fatal(err)
	}
	return oldJobs, oldImports, nextJobs, nextImports
}

func TestMetadataPairCommitImportScheduleAndRestart(t *testing.T) {
	root, key, jobID, importID := metadataTxnFixture(t)
	s, err := newStorage(root, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.setImportSchedule("owner", importID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, metadataJournalName)); !os.IsNotExist(err) {
		t.Fatalf("completed journal retained: %v", err)
	}
	next, err := newStorage(root, key)
	if err != nil {
		t.Fatal(err)
	}
	j, ok := next.get(jobID)
	if !ok || !j.AutoSync {
		t.Fatalf("archive schedule not durable: %+v", j)
	}
	imports := next.importsFor("owner")
	if len(imports) != 1 || !imports[0].AutoSync {
		t.Fatalf("import schedule not durable: %+v", imports)
	}
	other := next.all()
	for _, item := range other {
		if item.OwnerID == "other" && item.AutoSync {
			t.Fatal("unrelated owner schedule changed")
		}
	}
}

func TestMetadataJournalPreparedCrashRollsBackBothFiles(t *testing.T) {
	root, key, _, _ := metadataTxnFixture(t)
	oldJ, oldI, newJ, _ := makePendingMetadataTxn(t, root, "prepared")
	if err := writeSyncedMetadata(root, "jobs.json", newJ); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, key); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"jobs.json": oldJ, "imports.json": oldI} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("prepared crash did not roll back %s", name)
		}
	}
	if err := recoverMetadataPair(root); err != nil {
		t.Fatal("recovery should be idempotent:", err)
	}
}

func TestMetadataJournalCommittedCrashReplaysBothFiles(t *testing.T) {
	root, _, _, _ := metadataTxnFixture(t)
	_, _, newJ, newI := makePendingMetadataTxn(t, root, "committed")
	if err := writeSyncedMetadata(root, "jobs.json", newJ); err != nil {
		t.Fatal(err)
	}
	if err := recoverMetadataPair(root); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"jobs.json": newJ, "imports.json": newI} {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("committed crash did not replay %s", name)
		}
	}
	if _, err := os.Stat(filepath.Join(root, metadataJournalName)); !os.IsNotExist(err) {
		t.Fatalf("committed journal retained: %v", err)
	}
}

func TestMetadataJournalCorruptionAndSymlinkFailClosed(t *testing.T) {
	root, key, _, _ := metadataTxnFixture(t)
	before, _ := os.ReadFile(filepath.Join(root, "jobs.json"))
	if err := os.WriteFile(filepath.Join(root, metadataJournalName), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, key); err == nil {
		t.Fatal("corrupted journal was silently ignored")
	}
	after, _ := os.ReadFile(filepath.Join(root, "jobs.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("corrupt recovery modified source JSON")
	}
	if err := os.Remove(filepath.Join(root, metadataJournalName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("jobs.json", filepath.Join(root, metadataJournalName)); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, key); err == nil {
		t.Fatal("symlink journal was accepted")
	}
}

func TestMetadataJournalRejectsDamagedChecksumWithoutWriting(t *testing.T) {
	root, _, _, _ := metadataTxnFixture(t)
	oldJ, oldI, _, _ := makePendingMetadataTxn(t, root, "prepared")
	path := filepath.Join(root, metadataJournalName)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var txn metadataTransaction
	if err := json.Unmarshal(raw, &txn); err != nil {
		t.Fatal(err)
	}
	txn.Jobs.NewHash = strings.Repeat("0", 64)
	bad, _ := json.Marshal(txn)
	if err := writeAtomic(path, bad); err != nil {
		t.Fatal(err)
	}
	if err := recoverMetadataPair(root); err == nil {
		t.Fatal("damaged transaction checksum accepted")
	}
	actualJ, _ := os.ReadFile(filepath.Join(root, "jobs.json"))
	actualI, _ := os.ReadFile(filepath.Join(root, "imports.json"))
	if !bytes.Equal(actualJ, oldJ) || !bytes.Equal(actualI, oldI) {
		t.Fatal("damaged journal caused data changes")
	}
}

func TestMetadataRecoveryRejectsStaleJournalWithoutOverwritingNewerOwnerACL(t *testing.T) {
	for _, phase := range []string{"prepared", "committed"} {
		t.Run(phase, func(t *testing.T) {
			root, key, jobID, _ := metadataTxnFixture(t)
			_, oldImports, _, _ := makePendingMetadataTxn(t, root, phase)
			// Simulate another writer advancing an unrelated owner after the
			// journal was prepared. Neither file may be replayed or clobbered.
			jobsPath := filepath.Join(root, "jobs.json")
			raw, err := os.ReadFile(jobsPath)
			if err != nil {
				t.Fatal(err)
			}
			var records map[string]Job
			if err := json.Unmarshal(raw, &records); err != nil {
				t.Fatal(err)
			}
			job := records[jobID]
			job.SharedWith = []string{"third"}
			job.SharedForAll = true
			records[jobID] = job
			newer, err := json.MarshalIndent(records, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := writeAtomic(jobsPath, newer); err != nil {
				t.Fatal(err)
			}
			if err := recoverMetadataPair(root); err == nil {
				t.Fatal("stale journal silently overwrote newer owner ACL")
			}
			after, err := os.ReadFile(jobsPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, newer) {
				t.Fatal("stale journal changed newer owner ACL")
			}
			imports, err := os.ReadFile(filepath.Join(root, "imports.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(imports, oldImports) {
				t.Fatal("partial replay changed imports before noticing diverged jobs")
			}
			if _, err := os.Lstat(filepath.Join(root, metadataJournalName)); err != nil {
				t.Fatal("unsafe journal was discarded after recovery conflict")
			}
			if _, err := newStorage(root, key); err == nil {
				t.Fatal("server started with unresolved metadata recovery conflict")
			}
		})
	}
}

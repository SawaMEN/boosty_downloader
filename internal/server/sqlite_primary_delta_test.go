package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSQLPrimaryDeltaRoundTrip(t *testing.T) {
	old := []byte("{\n  \"a\": {\n    \"n\": 1\n  },\n  \"b\": 2,\n  \"keep\": true\n}")
	next := []byte("{\n  \"a\": {\n    \"n\": 3\n  },\n  \"c\": 4,\n  \"keep\": true\n}")
	img := metadataImage{Existed: true, Before: old, After: next, OldHash: metadataDigest(old), NewHash: metadataDigest(next)}
	delta, ok := sqlitePrimaryMakeDelta(img)
	if !ok || len(delta.Rows) != 3 {
		t.Fatalf("expected update, insert and delete: %+v", delta)
	}
	raw, err := json.Marshal(delta)
	if err != nil {
		t.Fatal(err)
	}
	var decoded sqlitePrimaryDeltaImage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, current := range [][]byte{old, next} {
		expanded, err := sqlitePrimaryExpandDelta(current, decoded)
		if err != nil || !bytes.Equal(expanded.Before, old) || !bytes.Equal(expanded.After, next) {
			t.Fatalf("delta reconstruction failed: %v", err)
		}
	}
	if _, err := sqlitePrimaryExpandDelta([]byte("{}"), decoded); err == nil {
		t.Fatal("unrelated mirror accepted")
	}
	delete(decoded.Rows, "a")
	if _, err := sqlitePrimaryExpandDelta(old, decoded); err == nil {
		t.Fatal("incomplete delta accepted")
	}
}

func TestSQLPrimaryDeltaJournalMixedMirrors(t *testing.T) {
	root := t.TempDir()
	old := []byte("{\n  \"a\": 1\n}")
	next := []byte("{\n  \"a\": 2\n}")
	img := metadataImage{Existed: true, Before: old, After: next, OldHash: metadataDigest(old), NewHash: metadataDigest(next)}
	journal := sqlitePrimaryTransaction{Version: sqlitePrimaryTransactionVersion, Jobs: img, Imports: img}
	raw, err := encodeSQLitePrimaryBestJournal(journal, sqlitePrimaryJournalMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	var header sqlitePrimaryDeltaJournal
	if err := json.Unmarshal(raw, &header); err != nil || header.Version != sqlitePrimaryDeltaVersion {
		t.Fatalf("expected v2 journal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "jobs.json"), old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "imports.json"), next, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := decodeSQLitePrimaryJournal(root, raw)
	if err != nil || !bytes.Equal(got.Jobs.After, next) || !bytes.Equal(got.Imports.Before, old) {
		t.Fatalf("mixed mirror recovery failed: %v", err)
	}
	journal.Jobs.Before = []byte("{ \"a\": 1 }")
	journal.Jobs.OldHash = metadataDigest(journal.Jobs.Before)
	legacy, err := encodeSQLitePrimaryBestJournal(journal, sqlitePrimaryJournalMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	got, err = decodeSQLitePrimaryJournal(root, legacy)
	if err != nil || !bytes.Equal(got.Jobs.Before, journal.Jobs.Before) {
		t.Fatalf("legacy format recovery failed: %v", err)
	}
}

func TestSQLPrimaryDeltaJournalSize(t *testing.T) {
	rows := make(map[string]string)
	for i := 0; i < 1000; i++ {
		rows[strings.Repeat("x", i+1)] = strings.Repeat("a", 128)
	}
	old, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	rows["x"] = "changed"
	next, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	img := metadataImage{Existed: true, Before: old, After: next, OldHash: metadataDigest(old), NewHash: metadataDigest(next)}
	journal := sqlitePrimaryTransaction{Version: sqlitePrimaryTransactionVersion, Jobs: img, Imports: img}
	raw, err := encodeSQLitePrimaryBestJournal(journal, 4096)
	if err != nil || len(raw) > 4096 {
		t.Fatalf("single-row delta exceeded limit: %d, %v", len(raw), err)
	}
	if _, err := encodeSQLitePrimaryJournal(journal, 4096); err == nil {
		t.Fatal("full images unexpectedly fit in delta limit")
	}
}

func TestSQLPrimaryDeltaSIGKILL(t *testing.T) {
	for _, tc := range []struct {
		phase string
		after bool
	}{
		{"prepared", false},
		{"sql-committed", true},
		{"json-committed", true},
	} {
		t.Run(tc.phase, func(t *testing.T) {
			root, _, _ := sqlPrimaryJournalFixture(t)
			jobs, err := sqliteShadowReadJobs(filepath.Join(root, "jobs.json"))
			if err != nil {
				t.Fatal(err)
			}
			imports, err := sqliteShadowReadImports(filepath.Join(root, "imports.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, mirror := range []struct {
				name  string
				value any
			}{
				{"jobs.json", jobs},
				{"imports.json", imports},
			} {
				raw, err := json.MarshalIndent(mirror.value, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, mirror.name), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSQLPrimaryJournalSIGKILLHelper$")
			cmd.Env = append(os.Environ(),
				"BD_TEST_SQL_PRIMARY_KILL=1",
				"BD_TEST_SQL_PRIMARY_ROOT="+root,
				"BD_TEST_SQL_PRIMARY_PHASE="+tc.phase)
			err = cmd.Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("expected SIGKILL: %v", err)
			}
			status, ok := exit.Sys().(syscall.WaitStatus)
			if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
				t.Fatalf("unexpected child exit: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(root, sqlitePrimaryTransactionName))
			if err != nil {
				t.Fatal(err)
			}
			var journal sqlitePrimaryDeltaJournal
			if err := json.Unmarshal(raw, &journal); err != nil || journal.Version != sqlitePrimaryDeltaVersion {
				t.Fatalf("child did not persist v2 intent: %v", err)
			}
			if err := recoverMetadataPair(root); err != nil {
				t.Fatal(err)
			}
			if err := recoverSQLitePrimaryCommitLocked(root); err != nil {
				t.Fatal(err)
			}
			sqlPrimaryAssertImages(t, root, tc.after)
		})
	}
}

package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDetachedSQLRecoveryRefusesToRewindNewerJSONRecords(t *testing.T) {
	for _, scenario := range []string{"newer-job", "newer-import", "missing-job", "missing-import"} {
		t.Run(scenario, func(t *testing.T) {
			root, _, export, _, id := detachedSQLRecoveryFixture(t)
			now := time.Now().UTC().Add(time.Hour)
			switch scenario {
			case "newer-job", "missing-job":
				path := filepath.Join(root, "jobs.json")
				jobs, err := sqliteShadowReadJobs(path)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "newer-job" {
					job := jobs[id]
					job.Title = "newer local JSON"
					job.UpdatedAt = now
					jobs[id] = job
				} else {
					job := jobs[id]
					job.ID = strings.Repeat("9", 24)
					job.PostID = "new-local-post"
					job.State = "queued"
					job.Title = "not in older SQL"
					job.CreatedAt = now
					job.UpdatedAt = now
					job.SharedWith = nil
					job.SharedForAll = false
					jobs[job.ID] = job
				}
				raw, err := json.Marshal(jobs)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "newer-import", "missing-import":
				path := filepath.Join(root, "imports.json")
				imports, err := sqliteShadowReadImports(path)
				if err != nil {
					t.Fatal(err)
				}
				id := strings.Repeat("a", 24)
				if scenario == "newer-import" {
					imp := imports[id]
					imp.State = "running"
					imp.UpdatedAt = now
					imports[id] = imp
				} else {
					imp := imports[id]
					imp.ID = strings.Repeat("b", 24)
					imp.Author = "new-local-author"
					imp.State = "queued"
					imp.CreatedAt = now
					imp.UpdatedAt = now
					imports[imp.ID] = imp
				}
				raw, err := json.Marshal(imports)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			full := filepath.Join(t.TempDir(), "newer-full")
			if err := PrepareFullDataSnapshot(root, full); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(t.TempDir(), "should-not-be-created")
			if err := RestoreDetachedSQLRecovery(full, export, out); err == nil {
				t.Fatal("older SQL was permitted to erase newer JSON metadata")
			}
			if _, err := os.Lstat(out); !os.IsNotExist(err) {
				t.Fatal("stale SQL recovery published an output directory")
			}
		})
	}
}

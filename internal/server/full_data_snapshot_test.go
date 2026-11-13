package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fullSnapshotTestFixture(t *testing.T) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "live-data")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"jobs.json":                     "{}",
		"imports.json":                  "{}",
		"accounts.json":                 "{\"accounts\":{}}",
		"authors.json":                  "{}",
		"settings/token.key":            "confidential-token",
		"library/user/author/post/index.html": "<h1>snapshot</h1>",
		"library/user/author/post/media/audio.bin": "original-media-contents",
		"revisions/user/author/post/version-1/manifest.json": "{\"version\":1}",
		"blobs/sha256/ab/abc":           "content-addressed-bytes",
	}
	for rel, content := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(t.TempDir(), "full-snapshot")
	return root, dest
}

func TestFullDataSnapshotCopiesMediaAndPrivateMetadata(t *testing.T) {
	root, dest := fullSnapshotTestFixture(t)
	if err := PrepareFullDataSnapshot(root, dest); err != nil {
		t.Fatalf("failed to snapshot entire stopped data directory: %v", err)
	}
	if err := VerifyFullDataSnapshot(dest); err != nil {
		t.Fatalf("full snapshot rejected by independent verifier: %v", err)
	}
	for _, rel := range []string{
		"jobs.json", "imports.json", "accounts.json", "authors.json",
		"settings/token.key", "library/user/author/post/index.html",
		"library/user/author/post/media/audio.bin",
		"revisions/user/author/post/version-1/manifest.json",
		"blobs/sha256/ab/abc",
	} {
		original, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		copiedPath := filepath.Join(dest, "data", rel)
		copied, err := os.ReadFile(copiedPath)
		if err != nil || !bytes.Equal(original, copied) {
			t.Fatalf("source and full snapshot differ for %s: %v", rel, err)
		}
		info, err := os.Lstat(copiedPath)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("full snapshot did not keep %s private: %v", rel, err)
		}
	}
	// The manifest includes original modes without exposing backup bytes
	// to users who lack permission to enter the snapshot root.
	raw, err := os.ReadFile(filepath.Join(dest, fullSnapshotManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest fullDataSnapshotManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != 1 || len(manifest.Entries) < 9 {
		t.Fatalf("full snapshot is incomplete: %+v", manifest)
	}
	if err := PrepareFullDataSnapshot(root, dest); err == nil {
		t.Fatal("existing full snapshot was overwritten")
	}
	if err := VerifyFullDataSnapshot(dest); err != nil {
		t.Fatalf("refused overwrite damaged prior snapshot: %v", err)
	}
}

func TestFullDataSnapshotRejectsCorruptOrIncompleteCopies(t *testing.T) {
	for _, scenario := range []string{"changed-bytes", "extra", "missing", "symlink", "world-readable", "traversal", "duplicate", "manifest"} {
		t.Run(scenario, func(t *testing.T) {
			root, dest := fullSnapshotTestFixture(t)
			if err := PrepareFullDataSnapshot(root, dest); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dest, "data", "library", "user", "author", "post", "index.html")
			manifestPath := filepath.Join(dest, fullSnapshotManifestName)
			switch scenario {
			case "changed-bytes":
				if err := os.WriteFile(path, []byte("different"), 0600); err != nil {
					t.Fatal(err)
				}
			case "extra":
				if err := os.WriteFile(filepath.Join(dest, "data", "untracked.bin"), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("index.html", path); err != nil {
					t.Fatal(err)
				}
			case "world-readable":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "manifest":
				if err := os.WriteFile(manifestPath, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "traversal", "duplicate":
				raw, err := os.ReadFile(manifestPath)
				if err != nil {
					t.Fatal(err)
				}
				var manifest fullDataSnapshotManifest
				if err := json.Unmarshal(raw, &manifest); err != nil {
					t.Fatal(err)
				}
				if scenario == "traversal" {
					manifest.Entries[0].Path = "../../outside"
				} else {
					manifest.Entries = append(manifest.Entries, manifest.Entries[0])
				}
				raw, err = json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(manifestPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := VerifyFullDataSnapshot(dest); err == nil {
				t.Fatalf("independent snapshot verifier accepted corruption: %s", scenario)
			}
		})
	}
}

func TestFullDataSnapshotRejectsLiveOrUnsafeSourceAndOutput(t *testing.T) {
	t.Run("locked", func(t *testing.T) {
		root, dest := fullSnapshotTestFixture(t)
		held, err := lockDataDirectory(root)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close()
		if err := PrepareFullDataSnapshot(root, dest); err == nil ||
			!strings.Contains(err.Error(), "locked") {
			t.Fatalf("snapshot ignored running server lock: %v", err)
		}
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Fatal("rejected busy snapshot created a destination")
		}
	})
	t.Run("output inside source", func(t *testing.T) {
		root, _ := fullSnapshotTestFixture(t)
		if err := PrepareFullDataSnapshot(root, filepath.Join(root, "copy")); err == nil {
			t.Fatal("snapshot would recurse inside live directory")
		}
	})
	t.Run("output symlink alias", func(t *testing.T) {
		root, _ := fullSnapshotTestFixture(t)
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(root, alias); err != nil {
			t.Fatal(err)
		}
		if err := PrepareFullDataSnapshot(root, filepath.Join(alias, "copy")); err == nil {
			t.Fatal("snapshot accepted physically nested symlink path")
		}
	})
	t.Run("source file symlink", func(t *testing.T) {
		root, dest := fullSnapshotTestFixture(t)
		path := filepath.Join(root, "settings", "token.key")
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(root, "jobs.json"), path); err != nil {
			t.Fatal(err)
		}
		if err := PrepareFullDataSnapshot(root, dest); err == nil {
			t.Fatal("snapshot followed source symlink")
		}
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Fatal("failed snapshot exposed a partial backup directory")
		}
	})
	t.Run("pending wal", func(t *testing.T) {
		root, dest := fullSnapshotTestFixture(t)
		if err := os.WriteFile(filepath.Join(root, "metadata.db-wal"), []byte("pending"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := PrepareFullDataSnapshot(root, dest); err == nil {
			t.Fatal("full snapshot copied uncheckpointed WAL")
		}
	})
	t.Run("pending journal", func(t *testing.T) {
		root, dest := fullSnapshotTestFixture(t)
		if err := os.WriteFile(filepath.Join(root, metadataJournalName), []byte("pending"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := PrepareFullDataSnapshot(root, dest); err == nil {
			t.Fatal("full snapshot copied uncommitted JSON")
		}
	})
	t.Run("sqlite journal", func(t *testing.T) {
		root, dest := fullSnapshotTestFixture(t)
		if err := os.WriteFile(filepath.Join(root, "metadata.db-journal"), []byte("pending"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := PrepareFullDataSnapshot(root, dest); err == nil {
			t.Fatal("full snapshot copied pending SQLite rollback journal")
		}
	})
}

func TestFullDataSnapshotVerifierRejectsUnsafeRoot(t *testing.T) {
	root, dest := fullSnapshotTestFixture(t)
	if err := PrepareFullDataSnapshot(root, dest); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dest, 0755); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFullDataSnapshot(dest); err == nil {
		t.Fatal("full snapshot verifier accepted a world-readable backup root")
	}
	if err := os.Chmod(dest, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "snapshot-alias")
	if err := os.Symlink(dest, alias); err != nil {
		t.Fatal(err)
	}
	if err := VerifyFullDataSnapshot(alias); err == nil {
		t.Fatal("full snapshot verifier followed a snapshot-root symlink")
	}
}

package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAbandonedRevisionStageCleanup(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "revisions", "owner", "author", "post")
	id := strings.Repeat("a", 24)
	published := filepath.Join(parent, id)
	stage := filepath.Join(parent, ".incomplete-"+id)
	for _, dir := range []string{published, stage, filepath.Join(parent, "unrelated")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	sentinel := filepath.Join(published, "keep")
	if err := os.WriteFile(sentinel, []byte("published"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(sentinel, filepath.Join(stage, "partial")); err != nil {
		t.Fatal(err)
	}
	if _, err := newStorage(root, testKey(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stage); !os.IsNotExist(err) {
		t.Fatalf("abandoned revision survived: %v", err)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "published" {
		t.Fatalf("published hardlink changed: %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(parent, "unrelated")); err != nil {
		t.Fatal("unrelated directory removed")
	}
}

func TestRevisionStageCleanupRefusesPendingJournal(t *testing.T) {
	root := t.TempDir()
	stage := filepath.Join(root, "revisions", "owner", "author", "post", ".incomplete-"+strings.Repeat("b", 24))
	if err := os.MkdirAll(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, sqliteHistoryJournalName), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupAbandonedRevisionStages(root); err == nil {
		t.Fatal("pending journal ignored")
	}
	if _, err := os.Stat(stage); err != nil {
		t.Fatal("journal-owned stage removed")
	}
}

func TestRevisionStageCleanupRejectsUnsafeEntries(t *testing.T) {
	for _, name := range []string{".incomplete-bad", ".incomplete-"+strings.Repeat("c", 24)} {
		t.Run(name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			parent := filepath.Join(root, "revisions", "owner", "author", "post")
			if err := os.MkdirAll(parent, 0700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(outside, "keep")
			if err := os.WriteFile(sentinel, []byte("private"), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, name)
			if name == ".incomplete-bad" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Symlink(outside, path); err != nil {
				t.Fatal(err)
			}
			if err := cleanupAbandonedRevisionStages(root); err == nil {
				t.Fatal("unsafe stage accepted")
			}
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("unsafe stage removed")
			}
			if got, err := os.ReadFile(sentinel); err != nil || string(got) != "private" {
				t.Fatal("foreign data changed")
			}
		})
	}
}

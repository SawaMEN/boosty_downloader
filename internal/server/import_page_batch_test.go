package server

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func importBatchFixture(t *testing.T, incremental bool) (*Storage, string, AuthorImport) {
	t.Helper()
	root, key := t.TempDir(), testKey(t)
	s, err := newStorage(root, key)
	if err != nil {
		t.Fatal(err)
	}
	v := AuthorImport{ID: "batch", OwnerID: "owner", Author: "creator", State: "queued",
		CreatedAt: time.Now().UTC(), Incremental: incremental, IncludeFiles: true}
	if _, err := s.createImports([]AuthorImport{v}); err != nil {
		t.Fatal(err)
	}
	v, ok := s.claimImport(v.ID)
	if !ok {
		t.Fatal("could not claim import")
	}
	return s, key, v
}

func TestImportPageAtomicCheckpointAndRestart(t *testing.T) {
	s, key, v := importBatchFixture(t, false)
	if err := s.upsert(Job{ID: "other", OwnerID: "other", Author: v.Author, PostID: "p0", State: "done"}); err != nil {
		t.Fatal(err)
	}
	feed := boosty.FeedResult{HasMore: true, NextOffset: "page-2"}
	for i := 0; i < 20; i++ {
		feed.Posts = append(feed.Posts, boosty.FeedPost{ID: fmt.Sprintf("p%d", i), HasAccess: true})
	}
	feed.Posts = append(feed.Posts, feed.Posts[0], boosty.FeedPost{ID: "locked"})
	next, ids, err := s.commitImportPage(v, feed, 100, false)
	if err != nil || len(ids) != 20 || next.Scanned != 22 || next.Queued != 20 || next.Skipped != 2 || next.NextOffset != "page-2" {
		t.Fatalf("invalid batch: %+v ids=%d err=%v", next, len(ids), err)
	}
	recovered, err := newStorage(s.root, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered.queued()) != 20 {
		t.Fatal("persisted jobs lost on restart")
	}
	resume, ok := recovered.claimImport(v.ID)
	if !ok || resume.NextOffset != "page-2" || resume.Scanned != 22 {
		t.Fatal("page checkpoint lost on restart")
	}
	last := boosty.FeedResult{Posts: []boosty.FeedPost{{ID: "p0", HasAccess: true}, {ID: "p20", HasAccess: true}}}
	done, ids, err := recovered.commitImportPage(resume, last, 100, false)
	if err != nil || done.State != "done" || done.Queued != 21 || done.Skipped != 3 || len(ids) != 1 {
		t.Fatalf("last batch: %+v %v", done, err)
	}
	restarted, err := newStorage(s.root, key)
	if err != nil || len(restarted.queuedImports()) != 0 || len(restarted.queued()) != 21 {
		t.Fatalf("terminal checkpoint replayed: %v", err)
	}
}

func TestImportPageRejectsCancellationAndRollsBack(t *testing.T) {
	for _, mode := range []string{"cancel", "disk", "cursor", "trash"} {
		t.Run(mode, func(t *testing.T) {
			s, _, v := importBatchFixture(t, false)
			feed := boosty.FeedResult{Posts: []boosty.FeedPost{{ID: "new", HasAccess: true}, {ID: "old", HasAccess: true}}, HasMore: true, NextOffset: "next"}
			switch mode {
			case "cancel":
				if err := s.cancelImport(v.OwnerID, v.ID); err != nil {
					t.Fatal(err)
				}
			case "disk":
				path := filepath.Join(s.root, "imports.json")
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "trash":
				now := time.Now().UTC()
				if err := s.upsert(Job{ID: "trash", OwnerID: v.OwnerID, Author: v.Author, PostID: "old", DeletedAt: &now, State: "done"}); err != nil {
					t.Fatal(err)
				}
			}
			before := s.importsFor(v.OwnerID)
			next, ids, err := s.commitImportPage(v, feed, 100, mode == "cursor")
			if err == nil || len(ids) != 0 || !reflect.DeepEqual(next, v) ||
				!reflect.DeepEqual(before, s.importsFor(v.OwnerID)) || len(s.queued()) != 0 {
				t.Fatalf("failed page changed state: %v", err)
			}
		})
	}
}

func TestImportPageIncrementalBoundary(t *testing.T) {
	s, _, v := importBatchFixture(t, true)
	if err := s.upsert(Job{ID: "known", OwnerID: v.OwnerID, Author: v.Author, PostID: "known", State: "done"}); err != nil {
		t.Fatal(err)
	}
	feed := boosty.FeedResult{Posts: []boosty.FeedPost{
		{ID: "new", HasAccess: true}, {ID: "known", HasAccess: true}, {ID: "older", HasAccess: true},
	}, HasMore: true, NextOffset: "next"}
	next, ids, err := s.commitImportPage(v, feed, 100, false)
	if err != nil || next.State != "done" || next.Scanned != 2 || next.Skipped != 1 || len(ids) != 1 {
		t.Fatalf("incremental boundary changed: %+v %v", next, err)
	}
}

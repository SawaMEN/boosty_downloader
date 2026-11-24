package server

import (
	"fmt"
	"testing"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

// Measure durable JSON writes, not CDN/network speed. Both paths begin
// with the same existing inventory and discover the same twenty posts.
// Fixture setup is outside the timed section.
func BenchmarkImportPagePersistence(b *testing.B) {
	for _, count := range []int{1000, 10000} {
		for _, mode := range []string{"per-post", "atomic-page"} {
			b.Run(fmt.Sprintf("%d/%s", count, mode), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					b.StopTimer()
					root := b.TempDir()
					key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
					s, err := newStorage(root, key)
					if err != nil {
						b.Fatal(err)
					}
					now := time.Now().UTC()
					v := AuthorImport{ID: "bench-import", OwnerID: "owner", Author: "creator", State: "running", CreatedAt: now}
					jobs := make([]Job, 0, count)
					for n := 0; n < count; n++ {
						id := fmt.Sprintf("old-%d", n)
						jobs = append(jobs, Job{ID: id, OwnerID: v.OwnerID, Author: v.Author, PostID: id, State: "done", CreatedAt: now})
					}
					s.mu.Lock()
					err = s.replaceStorageBatchLocked(jobs, []AuthorImport{v})
					s.mu.Unlock()
					if err != nil {
						b.Fatal(err)
					}
					feed := boosty.FeedResult{HasMore: true, NextOffset: "next"}
					for n := 0; n < 20; n++ {
						feed.Posts = append(feed.Posts, boosty.FeedPost{ID: fmt.Sprintf("new-%d", n), HasAccess: true})
					}
					b.StartTimer()
					if mode == "per-post" {
						for _, post := range feed.Posts {
							id, err := randomHex(12)
							if err != nil {
								b.Fatal(err)
							}
							_, created, err := s.createUniqueJob(Job{ID: id, OwnerID: v.OwnerID, Author: v.Author,
								PostID: post.ID, URL: boosty.PostURLFor(v.Author, post.ID), State: "queued", CreatedAt: now})
							if err != nil || !created {
								b.Fatalf("legacy persistence: %v", err)
							}
							v.Scanned++
							v.Queued++
						}
						if err := s.updateImport(v); err != nil {
							b.Fatal(err)
						}
						v.NextOffset = feed.NextOffset
						if err := s.updateImport(v); err != nil {
							b.Fatal(err)
						}
					} else {
						next, ids, err := s.commitImportPage(v, feed, importHardLimit, false)
						if err != nil || len(ids) != 20 || next.Scanned != 20 || next.NextOffset != "next" {
							b.Fatalf("batch persistence: %v", err)
						}
					}
					b.StopTimer()
					if got := s.queued(); len(got) != 20 {
						b.Fatalf("expected 20 queued jobs, got %d", len(got))
					}
				}
			})
		}
	}
}

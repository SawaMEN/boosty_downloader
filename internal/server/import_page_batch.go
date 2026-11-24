package server

import (
	"errors"
	"strings"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

// Commit one discovery page and its new jobs in the same durable metadata
// transaction. Never enqueue a job before persistence. The storage mutex
// serializes deduplication, import cancellation and the cursor checkpoint.
func (s *Storage) commitImportPage(current AuthorImport, feed boosty.FeedResult, limit int, invalidOffset bool) (AuthorImport, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return current, nil, err
	}
	previous := s.imports[current.ID]
	if previous == nil || previous.State == "canceled" {
		return current, nil, errImportCanceled
	}
	if previous.State != "running" || current.State != "running" ||
		previous.OwnerID != current.OwnerID || previous.Author != current.Author ||
		previous.NextOffset != current.NextOffset || previous.Scanned != current.Scanned ||
		previous.Queued != current.Queued || previous.Skipped != current.Skipped {
		return current, nil, errors.New("изменилось состояние импорта")
	}
	if limit <= 0 || limit > importHardLimit || len(feed.Posts) > importHardLimit {
		return current, nil, errors.New("недопустимый размер страницы импорта")
	}
	// Index only this author's posts, including trash. No persistent index
	// can become stale, and no other owner's archive affects deduplication.
	known := make(map[string]bool)
	for _, job := range s.jobs {
		if job.OwnerID == current.OwnerID && job.Author == current.Author {
			known[job.PostID] = known[job.PostID] || job.DeletedAt != nil
		}
	}
	next := cloneStorageImport(current)
	created := make([]Job, 0, min(len(feed.Posts), limit))
	ids := make([]string, 0, cap(created))
	batchIDs := make(map[string]bool)
	now := time.Now().UTC()
	stop := false
	for _, post := range feed.Posts {
		if next.Scanned >= limit {
			next.Limited = true
			stop = true
			break
		}
		next.Scanned++
		if !post.HasAccess {
			next.Skipped++
			continue
		}
		if !safeArchivePart(post.ID) || !boosty.ValidBlog(post.ID) {
			return current, nil, errors.New("недопустимый идентификатор поста")
		}
		if trashed, exists := known[post.ID]; exists {
			if trashed {
				return current, nil, errors.New("архив находится в корзине — сначала восстановите его")
			}
			next.Skipped++
			if next.Incremental {
				stop = true
				break
			}
			continue
		}
		id, err := randomHex(12)
		if err != nil {
			return current, nil, err
		}
		if _, exists := s.jobs[id]; exists || batchIDs[id] {
			return current, nil, errors.New("идентификатор архива уже существует")
		}
		batchIDs[id] = true
		known[post.ID] = false
		created = append(created, Job{
			ID: id, OwnerID: next.OwnerID, Author: next.Author, PostID: post.ID,
			URL: boosty.PostURLFor(next.Author, post.ID), Title: post.Title,
			State: "queued", CreatedAt: now, UpdatedAt: now,
			IncludeComments: next.IncludeComments, IncludeFiles: next.IncludeFiles,
			IncludeVideos: next.IncludeVideos, IncludeAudio: next.IncludeAudio,
			AutoSync: next.AutoSync, Subscription: next.Subscription,
		})
		ids = append(ids, id)
		next.Queued++
	}
	if next.Scanned >= limit && feed.HasMore {
		next.Limited = true
		stop = true
	}
	if stop || !feed.HasMore || feed.NextOffset == "" || len(feed.Posts) == 0 {
		next.State = "done"
		next.Error = ""
	} else {
		if invalidOffset || len(feed.NextOffset) > 200 || strings.ContainsAny(feed.NextOffset, "\r\n") || feed.NextOffset == current.NextOffset {
			return current, nil, errors.New("Boosty возвратил повторяющийся или неверный offset списка постов")
		}
		next.NextOffset = feed.NextOffset
	}
	next.UpdatedAt = now
	if err := s.replaceStorageBatchLocked(created, []AuthorImport{next}); err != nil {
		return current, nil, err
	}
	return next, ids, nil
}

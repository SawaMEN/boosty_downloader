package server

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type revisionRetentionPreview struct {
	Limit           int   `json:"limit"`
	Total           int   `json:"total"`
	Scanned         int   `json:"scanned"`
	Skipped         int   `json:"skipped"`
	CandidateCount  int   `json:"candidateCount"`
	LogicalBytes    int64 `json:"logicalBytes"`
	Complete        bool  `json:"complete"`
	PhysicalBytes   int64 `json:"physicalBytes"`
	PhysicalScanned int   `json:"physicalScanned"`
}

// Logical size counts all paths. The optional observer independently
// collects allocated blocks and hardlink counts without a second walk.
func revisionCandidateBytes(ctx context.Context, root, path string, observer *revisionBlockEstimate) (int64, error) {
	if info, err := purgeSafePath(root, path); err != nil || info == nil {
		return 0, errors.New("небезопасная или отсутствующая версия")
	}
	var size int64
	err := filepath.WalkDir(path, func(_ string, entry os.DirEntry, err error) error {
		if cancelErr := ctx.Err(); cancelErr != nil {
			return cancelErr
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("симлинк в истории")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || size > math.MaxInt64-info.Size() {
			return errors.New("небезопасный размер истории")
		}
		size += info.Size()
		observer.observe(info)
		return nil
	})
	return size, err
}

func (s *Storage) previewRevisionCandidates(ctx context.Context, job Job, limit int) (int, int64, error) {
	count, logical, _, _, err := s.previewRevisionUsage(ctx, job, limit)
	return count, logical, err
}

func (s *Storage) previewRevisionUsage(ctx context.Context, job Job, limit int) (int, int64, int64, bool, error) {
	if !validRevisionLimit(limit) {
		return 0, 0, 0, false, errors.New("некорректный лимит истории")
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, 0, false, err
	}
	versions, err := s.listArchiveRevisions(job)
	if err != nil || len(versions) <= limit {
		return 0, 0, 0, err == nil, err
	}
	observer := newRevisionBlockEstimate(s.root)
	var total int64
	for _, version := range versions[limit:] {
		n, err := revisionCandidateBytes(ctx, s.root, filepath.Join(s.revisionsDir(job), version.ID), observer)
		if err != nil {
			return 0, 0, 0, false, err
		}
		if total > math.MaxInt64-n {
			return 0, 0, 0, false, errors.New("переполнение размера истории")
		}
		total += n
	}
	physical, available := observer.bytes()
	return len(versions) - limit, total, physical, available, nil
}

func (a *App) adminRevisionPreview(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		fail(w, 403, "только администратор")
		return
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || !validRevisionLimit(limit) {
		fail(w, 400, "количество версий: от 1 до 64")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	// Snapshot active job identities under the storage mutex. Unlike all(),
	// an unavailable metadata generation must not look like an empty store.
	a.store.mu.Lock()
	if a.store.primaryFatal != nil || a.store.sqlitePrimaryGuardMutationLocked() != nil {
		a.store.mu.Unlock()
		fail(w, 503, "метаданные недоступны")
		return
	}
	jobs := make([]Job, 0, len(a.store.jobs))
	seen := make(map[[3]string]bool)
	for _, job := range a.store.jobs {
		if job != nil && job.DeletedAt == nil && (job.State == "done" || job.State == "partial") {
			key := [3]string{job.OwnerID, job.Author, job.PostID}
			if seen[key] {
				continue
			}
			seen[key] = true
			jobs = append(jobs, Job{ID: job.ID, OwnerID: job.OwnerID, Author: job.Author, PostID: job.PostID})
		}
	}
	a.store.mu.Unlock()
	result := revisionRetentionPreview{Limit: limit, Total: len(jobs)}
	for _, job := range jobs {
		if ctx.Err() != nil {
			break
		}
		unlock, ready := a.archiveGates.tryRead(job)
		if !ready {
			continue
		}
		current, ok := a.store.get(job.ID)
		if !ok || current.OwnerID != job.OwnerID || current.Author != job.Author ||
			current.PostID != job.PostID || (current.State != "done" && current.State != "partial") {
			unlock()
			continue
		}
		count, size, physical, available, err := a.store.previewRevisionUsage(ctx, job, limit)
		unlock()
		if err != nil || ctx.Err() != nil || result.LogicalBytes > math.MaxInt64-size {
			continue
		}
		result.Scanned++
		result.CandidateCount += count
		result.LogicalBytes += size
		if available && result.PhysicalBytes <= math.MaxInt64-physical {
			result.PhysicalBytes += physical
			result.PhysicalScanned++
		}
	}
	result.Skipped = result.Total - result.Scanned
	result.Complete = result.Skipped == 0
	if r.Context().Err() == nil {
		reply(w, 200, result)
	}
}

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
	MaxGiB          int   `json:"maxGiB"`
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

func (s *Storage) previewRevisionUsage(ctx context.Context, job Job, limit int, volume ...int) (int, int64, int64, bool, error) {
	_, maxBytes, err := revisionRetentionLimits(append([]int{limit}, volume...))
	if err != nil {
		return 0, 0, 0, false, errors.New("некорректный лимит истории")
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, 0, false, err
	}
	versions, err := s.listArchiveRevisions(job)
	if err != nil {
		return 0, 0, 0, false, err
	}
	keep, err := s.revisionKeepCount(ctx, job, versions, limit, maxBytes)
	if err != nil || len(versions) <= keep {
		return 0, 0, 0, err == nil, err
	}
	observer := newRevisionBlockEstimate(s.root)
	var total int64
	for _, version := range versions[keep:] {
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
	return len(versions) - keep, total, physical, available, nil
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
	maxGiB := 0
	if value := r.URL.Query().Get("maxGiB"); value != "" {
		maxGiB, err = strconv.Atoi(value)
		if err != nil || !validRevisionMaxGiB(maxGiB) {
			fail(w, 400, "объём истории: от 0 до 16384 ГиБ")
			return
		}
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
	jobs := make([]revisionPreviewIdentity, 0, len(a.store.jobs))
	seen := make(map[[3]string]bool)
	for _, job := range a.store.jobs {
		if job != nil && job.DeletedAt == nil && (job.State == "done" || job.State == "partial") {
			key := [3]string{job.OwnerID, job.Author, job.PostID}
			if seen[key] {
				continue
			}
			seen[key] = true
			jobs = append(jobs, newRevisionPreviewIdentity(*job))
		}
	}
	a.store.mu.Unlock()
	result := revisionRetentionPreview{Limit: limit, MaxGiB: maxGiB, Total: len(jobs)}
	for start := 0; start < len(jobs); start += revisionPreviewBatchSize {
		if ctx.Err() != nil {
			break
		}
		batch := jobs[start:min(start+revisionPreviewBatchSize, len(jobs))]
		if err := a.scanRevisionPreviewBatch(ctx, batch, limit, maxGiB, &result); err != nil {
			fail(w, 503, "метаданные недоступны")
			return
		}
	}
	result.Skipped = result.Total - result.Scanned
	result.Complete = result.Skipped == 0
	if r.Context().Err() == nil {
		reply(w, 200, result)
	}
}

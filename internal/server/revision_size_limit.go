package server

import (
	"context"
	"errors"
	"path/filepath"
)

func validRevisionMaxGiB(value int) bool {
	return value >= 0 && value <= 16384
}

func (p AdminPolicy) revisionMaxGiB() int {
	if p.RevisionMaxGiB == nil {
		return 0
	}
	return *p.RevisionMaxGiB
}

// Choose a newest-first prefix. Always retain one version, even when it
// alone exceeds the size cap. Never skip an expensive version in favour
// of a cheaper older one. A failed scan must not authorize deletion.
func revisionSizedPrefix(ctx context.Context, count, limit int, maxBytes int64, measure func(int) (int64, error)) (int, error) {
	if count < 0 || !validRevisionLimit(limit) || maxBytes < 0 {
		return 0, errors.New("некорректный лимит истории")
	}
	keep := min(count, limit)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if maxBytes == 0 || keep <= 1 {
		return keep, nil
	}
	remaining := maxBytes
	for i := 0; i < keep; i++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		size, err := measure(i)
		if err != nil {
			return 0, err
		}
		if size < 0 {
			return 0, errors.New("некорректный размер версии")
		}
		if size > remaining {
			return max(1, i), nil
		}
		remaining -= size
	}
	return keep, nil
}

func (s *Storage) revisionKeepCount(ctx context.Context, job Job, versions []ArchiveRevision, limit int, maxBytes int64) (int, error) {
	return revisionSizedPrefix(ctx, len(versions), limit, maxBytes, func(i int) (int64, error) {
		return revisionCandidateBytes(ctx, s.root, filepath.Join(s.revisionsDir(job), versions[i].ID), nil)
	})
}

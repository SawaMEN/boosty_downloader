package server

import (
	"context"
	"errors"
	"math"
	"time"
)

const revisionPreviewBatchSize = 32

// A detached identity, not a Job snapshot with borrowed ACLs or metadata.
type revisionPreviewIdentity struct {
	id, owner, author, post string
	created                 time.Time
}

func newRevisionPreviewIdentity(job Job) revisionPreviewIdentity {
	return revisionPreviewIdentity{
		id: job.ID, owner: job.OwnerID, author: job.Author, post: job.PostID,
		created: job.CreatedAt,
	}
}

func (i revisionPreviewIdentity) job() Job {
	return Job{ID: i.id, OwnerID: i.owner, Author: i.author, PostID: i.post, CreatedAt: i.created}
}

// Only for an admin aggregate preview, with the caller's archive read
// gates held. Perform a fresh complete parity audit for the whole batch.
// Never reuse the proof across batches or expose mutable storage records.
func (s *Storage) verifyRevisionPreviewBatch(batch []revisionPreviewIdentity) (uint32, error) {
	if len(batch) > revisionPreviewBatchSize {
		return 0, errors.New("revision preview batch too large")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sqlitePrimaryGuardMutationLocked(); err != nil {
		return 0, err
	}
	var valid uint32
	for n, identity := range batch {
		job := s.jobs[identity.id]
		if job == nil || job.ID != identity.id || job.DeletedAt != nil ||
			job.OwnerID != identity.owner || job.Author != identity.author || job.PostID != identity.post ||
			!job.CreatedAt.Equal(identity.created) || (job.State != "done" && job.State != "partial") {
			continue
		}
		valid |= uint32(1) << n
	}
	return valid, nil
}

type revisionPreviewMeasurement struct {
	count               int
	logical, physical   int64
	available, measured bool
}

func (a *App) scanRevisionPreviewBatch(ctx context.Context, batch []revisionPreviewIdentity, limit, maxGiB int, result *revisionRetentionPreview) error {
	var identities [revisionPreviewBatchSize]revisionPreviewIdentity
	var releases [revisionPreviewBatchSize]func()
	var measurements [revisionPreviewBatchSize]revisionPreviewMeasurement
	n := 0
	// Nonblocking locks avoid deadlock even when two physical archives
	// hash to the same gate. No Storage.mu is held while acquiring gates.
	defer func() {
		for _, release := range releases[:n] {
			release()
		}
	}()
	for _, identity := range batch {
		if ctx.Err() != nil {
			return nil
		}
		release, ready := a.archiveGates.tryRead(identity.job())
		if !ready {
			continue
		}
		identities[n], releases[n] = identity, release
		n++
	}
	if n == 0 {
		return nil
	}
	active := identities[:n]
	valid, err := a.store.verifyRevisionPreviewBatch(active)
	if err != nil {
		return err
	}
	for i, identity := range active {
		if ctx.Err() != nil {
			return nil
		}
		if valid&(uint32(1)<<i) == 0 {
			continue
		}
		count, logical, physical, available, err := a.store.previewRevisionUsage(ctx, identity.job(), limit, maxGiB)
		if err == nil && ctx.Err() == nil {
			measurements[i] = revisionPreviewMeasurement{
				count: count, logical: logical, physical: physical, available: available, measured: true,
			}
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	// Reject a changed identity or unrelated SQL corruption before any
	// measured values enter the aggregate. No cached generation proof.
	after, err := a.store.verifyRevisionPreviewBatch(active)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	for i, measured := range measurements[:n] {
		if valid&after&(uint32(1)<<i) == 0 || !measured.measured || result.LogicalBytes > math.MaxInt64-measured.logical {
			continue
		}
		result.Scanned++
		result.CandidateCount += measured.count
		result.LogicalBytes += measured.logical
		if measured.available && result.PhysicalBytes <= math.MaxInt64-measured.physical {
			result.PhysicalBytes += measured.physical
			result.PhysicalScanned++
		}
	}
	return nil
}

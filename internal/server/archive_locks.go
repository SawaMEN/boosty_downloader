package server

import (
	"hash/fnv"
	"sync"
)

// archiveLockShards bounds synchronization memory regardless of the number of
// authors, jobs, deleted archives or imported posts. Different jobs for the
// same owner/author/post share one on-disk archive and MUST use the same gate.
// Hash collisions only delay unrelated archives; they never weaken safety.
const archiveLockShards = 128

type archiveGates struct {
	stripes [archiveLockShards]sync.RWMutex
}

func (g *archiveGates) gate(job Job) *sync.RWMutex {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(job.OwnerID))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(job.Author))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(job.PostID))
	return &g.stripes[hash.Sum32()%archiveLockShards]
}

// Readers fail promptly while an internal refresh or restore is changing the
// archive; they must not hold open an HTTP connection for a multi-hour download.
func (g *archiveGates) tryRead(job Job) (func(), bool) {
	lock := g.gate(job)
	if !lock.TryRLock() {
		return nil, false
	}
	return lock.RUnlock, true
}

// The background download worker can wait for an in-flight ZIP or Range
// stream to finish before it touches the live archive. Only the archive writer
// may acquire this lock while modifying files or their revision history.
func (g *archiveGates) write(job Job) func() {
	lock := g.gate(job)
	lock.Lock()
	return lock.Unlock
}

func (g *archiveGates) tryWrite(job Job) (func(), bool) {
	lock := g.gate(job)
	if !lock.TryLock() {
		return nil, false
	}
	return lock.Unlock, true
}

// tryWriteAll is used only for bulk storage purges, which can select any
// number of physical posts. Taking every stripe in a deterministic order
// prevents a new matching job from appearing outside the locked set. If even
// one archive is busy, release all acquired locks and fail without deleting
// anything. No waiters, unbounded maps or cross-stripe deadlocks are created.
func (g *archiveGates) tryWriteAll() (func(), bool) {
	for i := range g.stripes {
		if !g.stripes[i].TryLock() {
			for j := i - 1; j >= 0; j-- {
				g.stripes[j].Unlock()
			}
			return nil, false
		}
	}
	return func() {
		for i := len(g.stripes) - 1; i >= 0; i-- {
			g.stripes[i].Unlock()
		}
	}, true
}

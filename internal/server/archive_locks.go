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

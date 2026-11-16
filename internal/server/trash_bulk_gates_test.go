package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Direct Storage callers, not only HTTP and background sweeps, must honor
// active readers. A bulk purge has no fixed set of paths ahead of the Storage
// metadata lock, so it reserves every bounded archive stripe or fails closed.
func TestStorageBulkPurgeCannotDeleteBusyArchive(t *testing.T) {
	store, err := newStorage(t.TempDir(), testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	first := trashTestJob(t, store, strings.Repeat("1", 24))
	second := trashTestJob(t, store, strings.Repeat("2", 24))
	for _, job := range []Job{first, second} {
		if n, err := store.removeOwned(job.OwnerID, job.ID, ""); n != 1 || err != nil {
			t.Fatalf("cannot trash %s: %d %v", job.ID, n, err)
		}
	}
	readerUnlock, acquired := store.gates.tryRead(first)
	if !acquired {
		t.Fatal("cannot acquire reader before bulk purge")
	}
	if n, err := store.purgeOwnedBefore("admin", "", "", time.Time{}); n != 0 || err == nil {
		readerUnlock()
		t.Fatalf("bulk Storage purge deleted a busy archive: %d %v", n, err)
	}
	if len(store.trashed("admin")) != 2 {
		readerUnlock()
		t.Fatal("a refused bulk purge deleted metadata")
	}
	for _, job := range []Job{first, second} {
		path := filepath.Join(store.archiveDir(job.OwnerID, job.Author, job.PostID), "index.html")
		if _, err := os.Stat(path); err != nil {
			readerUnlock()
			t.Fatalf("bulk purge deleted %s: %v", path, err)
		}
	}

	// A failed all-shards acquisition must release previously obtained gates.
	// An unrelated request must still be able to make progress.
	readerUnlock()
	if n, err := store.purgeOwnedBefore("admin", "", "", time.Time{}); n != 2 || err != nil {
		t.Fatalf("bulk purge did not recover after reader finished: %d %v", n, err)
	}
	for _, job := range []Job{first, second} {
		path := store.archiveDir(job.OwnerID, job.Author, job.PostID)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("purged archive still exists: %s %v", path, err)
		}
	}
}

func TestStorageBulkPurgeRollbackReleasesAcquiredStripes(t *testing.T) {
	var gates archiveGates
	last := &gates.stripes[archiveLockShards-1]
	last.RLock()
	if unlock, ok := gates.tryWriteAll(); ok {
		unlock()
		last.RUnlock()
		t.Fatal("bulk purge got the all-stripe lock while last stripe had a reader")
	}
	// The first stripes were acquired and subsequently rolled back.
	for i := 0; i < archiveLockShards-1; i++ {
		if !gates.stripes[i].TryLock() {
			last.RUnlock()
			t.Fatalf("stripe %d was left locked after a failed bulk acquisition", i)
		}
		gates.stripes[i].Unlock()
	}
	last.RUnlock()
	unlock, ok := gates.tryWriteAll()
	if !ok {
		t.Fatal("bulk purge did not recover after busy reader released its gate")
	}
	unlock()
}

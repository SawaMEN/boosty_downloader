package server

import (
	"math"
	"os"
	"syscall"
)

const revisionEstimateMaxInodes = 65536

type revisionEstimateInode struct {
	links uint64
	seen  uint64
	bytes int64
}

// One publication at a time: no unbounded global map and no claims
// about shared inodes across independently scanned archives.
type revisionBlockEstimate struct {
	device uint64
	inodes map[uint64]revisionEstimateInode
	limit  int
	failed bool
}

func newRevisionBlockEstimate(root string) *revisionBlockEstimate {
	e := &revisionBlockEstimate{limit: revisionEstimateMaxInodes}
	info, err := os.Lstat(root)
	if err != nil {
		e.failed = true
		return e
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() {
		e.failed = true
		return e
	}
	e.device = uint64(st.Dev)
	e.inodes = make(map[uint64]revisionEstimateInode)
	return e
}

func (e *revisionBlockEstimate) invalidate() {
	e.failed = true
	e.inodes = nil
}

func (e *revisionBlockEstimate) observe(info os.FileInfo) {
	if e == nil || e.failed {
		return
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Nlink == 0 || st.Blocks < 0 || st.Blocks > math.MaxInt64/512 {
		e.invalidate()
		return
	}
	// A different mounted filesystem cannot contribute to the data
	// volume's available bytes.
	if uint64(st.Dev) != e.device {
		return
	}
	key := uint64(st.Ino)
	links, allocated := uint64(st.Nlink), int64(st.Blocks)*512
	item, exists := e.inodes[key]
	if !exists {
		if len(e.inodes) >= e.limit {
			e.invalidate()
			return
		}
		item = revisionEstimateInode{links: links, bytes: allocated}
	}
	if item.links != links || item.bytes != allocated || item.seen >= links {
		e.invalidate()
		return
	}
	item.seen++
	e.inodes[key] = item
}

func (e *revisionBlockEstimate) bytes() (int64, bool) {
	if e == nil || e.failed {
		return 0, false
	}
	var total int64
	for _, item := range e.inodes {
		if item.seen != item.links {
			continue
		}
		if total > math.MaxInt64-item.bytes {
			return 0, false
		}
		total += item.bytes
	}
	return total, true
}

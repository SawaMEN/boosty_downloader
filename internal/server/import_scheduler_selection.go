package server

import (
	"container/heap"
	"sort"
	"strings"
	"time"
)

type importScheduleGroup struct {
	latest *AuthorImport
	busy   bool
}

func importScheduleBefore(a, b *AuthorImport) bool {
	if a.UpdatedAt.Equal(b.UpdatedAt) {
		return a.ID < b.ID
	}
	return a.UpdatedAt.Before(b.UpdatedAt)
}

// The root is the newest (worst) retained candidate. Only the requested
// number of due imports is retained, regardless of the library size.
type importScheduleHeap []*AuthorImport

func (h importScheduleHeap) Len() int { return len(h) }
func (h importScheduleHeap) Less(i, j int) bool {
	return importScheduleBefore(h[j], h[i])
}
func (h importScheduleHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *importScheduleHeap) Push(x any) {
	*h = append(*h, x.(*AuthorImport))
}
func (h *importScheduleHeap) Pop() any {
	last := len(*h) - 1
	x := (*h)[last]
	(*h)[last] = nil
	*h = (*h)[:last]
	return x
}

// The caller owns Storage.mu. The selected pointers are borrowed only
// until the caller copies them into the atomic metadata batch.
func selectDueImports(records map[string]*AuthorImport, now time.Time, interval time.Duration, limit int, allowed func(string) bool) []*AuthorImport {
	if interval <= 0 || limit <= 0 {
		return nil
	}
	groups := make(map[string]importScheduleGroup)
	for _, v := range records {
		key := v.OwnerID + "/" + strings.ToLower(v.Author)
		group := groups[key]
		old := group.latest
		if old == nil || old.CreatedAt.Before(v.CreatedAt) || (old.CreatedAt.Equal(v.CreatedAt) && old.ID < v.ID) {
			group.latest = v
		}
		group.busy = group.busy || v.State == "queued" || v.State == "running"
		groups[key] = group
	}
	selected := make(importScheduleHeap, 0, min(limit, len(groups)))
	for _, group := range groups {
		v := group.latest
		if group.busy || !(v.State == "done" || (v.State == "failed" && v.Incremental)) || !v.AutoSync ||
			v.UpdatedAt.IsZero() || now.Sub(v.UpdatedAt) < interval || (allowed != nil && !allowed(v.OwnerID)) {
			continue
		}
		if len(selected) < limit {
			heap.Push(&selected, v)
		} else if importScheduleBefore(v, selected[0]) {
			selected[0] = v
			heap.Fix(&selected, 0)
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		return importScheduleBefore(selected[i], selected[j])
	})
	return selected
}

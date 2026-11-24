package server

import (
	"container/heap"
	"sort"
	"strings"
	"time"
)

// For shallow pages retain only the requested sorted prefix. Deep pages use
// the full sort rather than maintaining an increasingly expensive heap.
// The authoritative inventory and ACL checks remain outside this selector.
const catalogHeapLimit = 4096

type catalogRankedItem struct {
	item catalogItem
	text string
	date time.Time
}

type catalogRankHeap struct {
	values []catalogRankedItem
	asc    bool
	text   bool
}

func (h catalogRankHeap) before(a, b catalogRankedItem) bool {
	var cmp int
	if h.text {
		cmp = strings.Compare(a.text, b.text)
	} else {
		cmp = a.date.Compare(b.date)
	}
	if cmp == 0 {
		return a.item.ID < b.item.ID
	}
	if h.asc {
		return cmp < 0
	}
	return cmp > 0
}

// Heap root is the worst retained item, not the first item in display order.
func (h catalogRankHeap) Len() int { return len(h.values) }
func (h catalogRankHeap) Less(i, j int) bool {
	return h.before(h.values[j], h.values[i])
}
func (h catalogRankHeap) Swap(i, j int) {
	h.values[i], h.values[j] = h.values[j], h.values[i]
}
func (h *catalogRankHeap) Push(x any) {
	h.values = append(h.values, x.(catalogRankedItem))
}
func (h *catalogRankHeap) Pop() any {
	last := len(h.values) - 1
	x := h.values[last]
	h.values[last] = catalogRankedItem{}
	h.values = h.values[:last]
	return x
}

type catalogSelection struct {
	page, perPage, total, limit int
	orderBy                    string
	rank                       catalogRankHeap
}

func newCatalogSelection(page, perPage int, orderBy, direction string) *catalogSelection {
	limit := page * perPage
	if limit > catalogHeapLimit {
		limit = 0
	}
	return &catalogSelection{
		page: page, perPage: perPage, limit: limit, orderBy: orderBy,
		rank: catalogRankHeap{asc: direction == "asc", text: orderBy == "title" || orderBy == "author"},
	}
}

func (s *catalogSelection) add(item catalogItem) {
	s.total++
	r := catalogRankedItem{item: item}
	switch s.orderBy {
	case "title":
		r.text = strings.ToLower(item.Title)
	case "author":
		r.text = strings.ToLower(item.Author)
	case "created":
		r.date = item.CreatedAt
	default:
		r.date = catalogDate(item.Job)
	}
	if s.limit == 0 {
		s.rank.values = append(s.rank.values, r)
		return
	}
	if s.rank.Len() < s.limit {
		heap.Push(&s.rank, r)
		return
	}
	if s.rank.before(r, s.rank.values[0]) {
		s.rank.values[0] = r
		heap.Fix(&s.rank, 0)
	}
}

// Called once after scanning all eligible rows. Total is exact, including
// rows outside the retained prefix. Empty pages serialize as [].
func (s *catalogSelection) result() catalogPage {
	sort.Slice(s.rank.values, func(i, j int) bool {
		return s.rank.before(s.rank.values[i], s.rank.values[j])
	})
	start := min((s.page-1)*s.perPage, len(s.rank.values))
	end := min(start+s.perPage, len(s.rank.values))
	items := make([]catalogItem, 0, end-start)
	for _, r := range s.rank.values[start:end] {
		items = append(items, r.item)
	}
	return catalogPage{
		Items: items, Total: s.total, Page: s.page, PerPage: s.perPage,
		Pages: (s.total + s.perPage - 1) / s.perPage,
	}
}

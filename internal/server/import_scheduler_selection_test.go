package server

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// Reference implementation deliberately uses a full sort and the original
// two-map grouping algorithm. Equal timestamps now have a stable ID order.
func referenceDueImports(records map[string]*AuthorImport, now time.Time, interval time.Duration, limit int, allowed func(string) bool) []*AuthorImport {
	latest := map[string]*AuthorImport{}
	busy := map[string]bool{}
	for _, v := range records {
		key := v.OwnerID + "/" + strings.ToLower(v.Author)
		if old := latest[key]; old == nil || old.CreatedAt.Before(v.CreatedAt) || (old.CreatedAt.Equal(v.CreatedAt) && old.ID < v.ID) {
			latest[key] = v
		}
		if v.State == "queued" || v.State == "running" {
			busy[key] = true
		}
	}
	out := make([]*AuthorImport, 0, len(latest))
	for key, v := range latest {
		if !busy[key] && (v.State == "done" || (v.State == "failed" && v.Incremental)) && v.AutoSync &&
			(allowed == nil || allowed(v.OwnerID)) && !v.UpdatedAt.IsZero() && now.Sub(v.UpdatedAt) >= interval {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return importScheduleBefore(out[i], out[j])
	})
	return out[:min(limit, len(out))]
}

func importSchedulerFixture(n int, now time.Time) map[string]*AuthorImport {
	records := make(map[string]*AuthorImport, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%024x", i)
		v := &AuthorImport{ID: id, OwnerID: fmt.Sprintf("owner-%d", i%11),
			Author: fmt.Sprintf("author-%d", i%997), State: "done", AutoSync: i%7 != 0,
			CreatedAt: now.Add(-time.Duration(i%23) * time.Hour),
			UpdatedAt: now.Add(-time.Duration(i%19) * time.Hour)}
		if i%13 == 0 {
			v.State = "running"
		} else if i%17 == 0 {
			v.State = "failed"
			v.Incremental = i%2 == 0
		}
		if i%3 == 0 {
			v.Author = strings.ToUpper(v.Author)
		}
		records[id] = v
	}
	return records
}

func TestImportSchedulerBoundedSelection(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	records := importSchedulerFixture(25000, now)
	allowed := func(owner string) bool { return owner != "owner-3" }
	for _, limit := range []int{0, 1, 10, 64, 1000, 30000} {
		got := selectDueImports(records, now, time.Hour, limit, allowed)
		want := referenceDueImports(records, now, time.Hour, limit, allowed)
		if len(got) != len(want) {
			t.Fatalf("limit %d: got %d want %d", limit, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("limit %d: result %d differs", limit, i)
			}
		}
	}
}

func BenchmarkImportSchedulerSelection(b *testing.B) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	records := importSchedulerFixture(100000, now)
	b.Run("full-sort", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = referenceDueImports(records, now, time.Hour, 10, nil)
		}
	})
	b.Run("bounded", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = selectDueImports(records, now, time.Hour, 10, nil)
		}
	})
}

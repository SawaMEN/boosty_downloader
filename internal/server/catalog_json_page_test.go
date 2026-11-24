package server

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func jsonCatalogFixture(n int) *Storage {
	s := &Storage{jobs: make(map[string]*Job, n)}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%024x", i)
		j := &Job{
			ID: id, OwnerID: "other", Author: "Creator", PostID: id,
			Title: fmt.Sprintf("Post %04d", i%173), State: "done",
			CreatedAt: time.Unix(int64(i%101), 0), SharedWith: []string{"viewer"},
		}
		if i%3 == 0 {
			j.OwnerID = "viewer"
		}
		if i%7 == 0 {
			j.SharedWith = []string{"private"}
		}
		if i%11 == 0 {
			at := time.Unix(1, 0)
			j.DeletedAt = &at
		}
		if i%13 == 0 {
			j.State = "queued"
		}
		s.jobs[id] = j
	}
	return s
}

func detachedCatalogPage(s *Storage, filter catalogFilter, page, perPage int) catalogPage {
	selection := newCatalogSelection(page, perPage, "title", "asc")
	for _, job := range s.all() {
		if item, ok := filter.item(job); ok {
			selection.add(item)
		}
	}
	return selection.result()
}

func TestCatalogJSONPageParityAndDetachedResults(t *testing.T) {
	s := jsonCatalogFixture(1000)
	for _, scope := range []string{"mine", "shared"} {
		for _, query := range []string{"", "post 00", "other-name"} {
			filter := catalogFilter{viewer: "viewer", scope: scope, query: query,
				ownerNames: map[string]string{"other": "Other-name"}}
			for _, page := range []int{1, 3, 50} {
				selection := newCatalogSelection(page, 20, "title", "asc")
				got, ok := s.catalogJSONPage(filter, selection)
				want := detachedCatalogPage(s, filter, page, 20)
				if !ok || !reflect.DeepEqual(got, want) {
					t.Fatalf("catalog mismatch: scope=%s query=%s page=%d", scope, query, page)
				}
				if len(selection.rank.values) != 0 {
					t.Fatal("borrowed ranking references retained")
				}
				for _, item := range got.Items {
					if scope == "shared" && (item.OwnerID != "" || item.SharedWith != nil) {
						t.Fatal("shared metadata leaked")
					}
					if len(item.SharedWith) != 0 {
						item.SharedWith[0] = "changed"
						if s.jobs[item.ID].SharedWith[0] == "changed" {
							t.Fatal("page aliases storage")
						}
					}
				}
			}
		}
	}
	if _, ok := s.catalogJSONPage(catalogFilter{}, newCatalogSelection(1000, 20, "title", "asc")); ok {
		t.Fatal("deep page should use detached fallback")
	}
}

func BenchmarkCatalogJSONPage(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		s := jsonCatalogFixture(n)
		filter := catalogFilter{viewer: "viewer", scope: "mine"}
		for _, mode := range []string{"snapshot", "direct"} {
			b.Run(fmt.Sprintf("%d/%s", n, mode), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var result catalogPage
					if mode == "snapshot" {
						result = detachedCatalogPage(s, filter, 1, 20)
					} else {
						result, _ = s.catalogJSONPage(filter, newCatalogSelection(1, 20, "title", "asc"))
					}
					if len(result.Items) != 20 || result.Total < 20 {
						b.Fatal("invalid result")
					}
				}
			})
		}
	}
}

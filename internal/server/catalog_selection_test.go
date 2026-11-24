package server

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func catalogScaleFixture(n int) []catalogItem {
	items := make([]catalogItem, n)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range items {
		items[i] = catalogItem{Job: Job{
			ID: fmt.Sprintf("%024x", i), Title: fmt.Sprintf("Название %03d", i%127),
			Author: fmt.Sprintf("Author-%02d", i%17),
			CreatedAt: base.Add(time.Duration(i%43) * time.Minute),
			UpdatedAt: base.Add(time.Duration(i%53) * time.Minute),
		}}
		if i%2 == 0 {
			items[i].ArchiveUpdatedAt = base.Add(time.Duration(i%31) * time.Hour)
		}
	}
	rand.New(rand.NewSource(42)).Shuffle(n, func(i, j int) { items[i], items[j] = items[j], items[i] })
	return items
}

// Independent full-sort reference mirrors the original catalog ordering.
func referenceCatalogPage(input []catalogItem, page, perPage int, orderBy, direction string) catalogPage {
	items := append([]catalogItem{}, input...)
	sort.Slice(items, func(i, j int) bool {
		left, right := items[i], items[j]
		var cmp int
		switch orderBy {
		case "created":
			cmp = left.CreatedAt.Compare(right.CreatedAt)
		case "title":
			cmp = strings.Compare(strings.ToLower(left.Title), strings.ToLower(right.Title))
		case "author":
			cmp = strings.Compare(strings.ToLower(left.Author), strings.ToLower(right.Author))
		default:
			cmp = catalogDate(left.Job).Compare(catalogDate(right.Job))
		}
		if cmp == 0 {
			return left.ID < right.ID
		}
		if direction == "asc" {
			return cmp < 0
		}
		return cmp > 0
	})
	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	return catalogPage{Items: items[start:end], Total: len(items), Page: page, PerPage: perPage,
		Pages: (len(items) + perPage - 1) / perPage}
}

func TestCatalogSelectionMatchesFullSort(t *testing.T) {
	for _, n := range []int{0, 1, 51, 5000} {
		items := catalogScaleFixture(n)
		for _, order := range []string{"updated", "created", "title", "author"} {
			for _, direction := range []string{"asc", "desc"} {
				for _, page := range []int{1, 2, 10, 204, 205, 1000000} {
					s := newCatalogSelection(page, 20, order, direction)
					for _, item := range items {
						s.add(item)
					}
					if s.limit > 0 && len(s.rank.values) > s.limit {
						t.Fatal("bounded selection exceeded its limit")
					}
					if got, want := s.result(), referenceCatalogPage(items, page, 20, order, direction); !reflect.DeepEqual(got, want) {
						t.Fatalf("page differs: n=%d order=%s direction=%s page=%d", n, order, direction, page)
					}
				}
			}
		}
	}
}

func BenchmarkCatalogSelection(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		items := catalogScaleFixture(n)
		for _, mode := range []string{"full-sort", "bounded"} {
			b.Run(fmt.Sprintf("%d/%s", n, mode), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var result catalogPage
					if mode == "full-sort" {
						result = referenceCatalogPage(items, 1, 20, "title", "asc")
					} else {
						s := newCatalogSelection(1, 20, "title", "asc")
						for _, item := range items {
							s.add(item)
						}
						result = s.result()
					}
					if result.Total != n || len(result.Items) != 20 {
						b.Fatal("incorrect benchmark result")
					}
				}
			})
		}
	}
}

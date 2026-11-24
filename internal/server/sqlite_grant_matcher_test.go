package server

import (
	"fmt"
	"slices"
	"testing"
)

func TestSQLiteGrantMatcher(t *testing.T) {
	for _, n := range []int{0, 1, 2, sqliteSmallGrantLimit, sqliteSmallGrantLimit + 1, 256} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			grants := make([]string, n)
			for i := range grants {
				grants[i] = fmt.Sprintf("member-%d", i)
			}
			original := slices.Clone(grants)
			if err := validateSQLiteGrants(grants); err != nil {
				t.Fatal(err)
			}
			matcher := newSQLiteGrantMatcher(grants)
			if matcher.consume("unexpected") || matcher.complete() != (n == 0) {
				t.Fatal("unexpected grant or incorrect empty state")
			}
			for i := n - 1; i >= 0; i-- {
				if !matcher.consume(grants[i]) || matcher.consume(grants[i]) {
					t.Fatal("valid or duplicate SQL grant handled incorrectly")
				}
				if matcher.complete() != (i == 0) {
					t.Fatal("missing grant not detected")
				}
			}
			if !matcher.complete() || !slices.Equal(original, grants) {
				t.Fatal("grant order changed or incomplete match")
			}
			if n > 0 {
				duplicate := append(slices.Clone(grants), grants[0])
				if validateSQLiteGrants(duplicate) == nil {
					t.Fatal("duplicate payload grant accepted")
				}
				invalid := slices.Clone(grants)
				invalid[n-1] = "../unsafe"
				if validateSQLiteGrants(invalid) == nil {
					t.Fatal("unsafe payload grant accepted")
				}
			}
		})
	}
}

func BenchmarkSQLiteGrantMatcher(b *testing.B) {
	for _, n := range []int{1, 8, 32} {
		grants := make([]string, n)
		for i := range grants {
			grants[i] = fmt.Sprintf("member-%d", i)
		}
		b.Run(fmt.Sprintf("%d/map", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				expected := make(map[string]bool, len(grants))
				for _, member := range grants {
					expected[member] = true
				}
				for _, member := range grants {
					if !expected[member] {
						b.Fatal("missing grant")
					}
					delete(expected, member)
				}
				if len(expected) != 0 {
					b.Fatal("unconsumed grant")
				}
			}
		})
		b.Run(fmt.Sprintf("%d/matcher", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				matcher := newSQLiteGrantMatcher(grants)
				for _, member := range grants {
					if !matcher.consume(member) {
						b.Fatal("missing grant")
					}
				}
				if !matcher.complete() {
					b.Fatal("unconsumed grant")
				}
			}
		})
	}
}

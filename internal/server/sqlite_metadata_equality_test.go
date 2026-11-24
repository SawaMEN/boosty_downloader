package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/SawaMEN/boosty_downloader/internal/boosty"
)

func legacySQLiteMetadataEqual(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && (bytes.Equal(left, right) || sqliteSameJSON(left, right))
}

func TestSQLiteMetadataEqualityFastPathParity(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := Job{ID: "job", OwnerID: "owner", Author: "creator", PostID: "post",
		CreatedAt: now, UpdatedAt: now, SharedWith: []string{"member"},
		RequiredTier: &boosty.PostRequirement{TierPrice: 10}}
	changed := cloneStorageJob(base)
	changed.SharedWith[0] = "different"
	empty := base
	empty.SharedWith = []string{}
	nilGrant := empty
	nilGrant.SharedWith = nil
	invalidTime := base
	invalidTime.CreatedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	invalidPrice := cloneStorageJob(base)
	invalidPrice.RequiredTier.TierPrice = math.Inf(1)
	imp := AuthorImport{ID: "import", OwnerID: "owner", Author: "creator", CreatedAt: now, UpdatedAt: now}
	invalidImport := imp
	invalidImport.UpdatedAt = invalidTime.CreatedAt
	for _, tc := range []struct {
		name string
		a    any
		b    any
	}{
		{"equal-job", base, cloneStorageJob(base)},
		{"changed-acl", base, changed},
		{"empty-omitempty", empty, nilGrant},
		{"invalid-time", invalidTime, invalidTime},
		{"invalid-price", invalidPrice, invalidPrice},
		{"equal-import", imp, imp},
		{"invalid-import", invalidImport, invalidImport},
		{"different-type", base, imp},
		{"generic-map", map[string]int{"x": 1}, map[string]int{"x": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := sqliteShadowEqual(tc.a, tc.b), legacySQLiteMetadataEqual(tc.a, tc.b); got != want {
				t.Fatalf("equality changed: got=%v want=%v", got, want)
			}
		})
	}
}

func BenchmarkSQLiteMetadataEquality(b *testing.B) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	left := Job{ID: "archive", OwnerID: "owner", Author: "creator", PostID: "post",
		Title: "Archived media", State: "done", CreatedAt: now, UpdatedAt: now,
		SharedWith: []string{"member"}, RequiredTier: &boosty.PostRequirement{TierPrice: 10}}
	right := cloneStorageJob(left)
	for _, mode := range []string{"legacy", "fast-path"} {
		b.Run(fmt.Sprintf("equal-job/%s", mode), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var ok bool
				if mode == "legacy" {
					ok = legacySQLiteMetadataEqual(left, right)
				} else {
					ok = sqliteShadowEqual(left, right)
				}
				if !ok {
					b.Fatal("equal metadata rejected")
				}
			}
		})
	}
}

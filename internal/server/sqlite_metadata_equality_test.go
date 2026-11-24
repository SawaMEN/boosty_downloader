package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
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

// Audit the concrete metadata schema before a fast proof can bypass JSON.
// All currently fallible JSON fields must appear in the explicit validator.
// New custom encoders, unsupported kinds, time fields or floats fail here.
func TestSQLiteMetadataFastPathSchema(t *testing.T) {
	timeType := reflect.TypeOf(time.Time{})
	marshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshaler := reflect.TypeOf((*interface{ MarshalText() ([]byte, error) })(nil)).Elem()
	var risky []string
	var inspect func(reflect.Type, string)
	inspect = func(typ reflect.Type, path string) {
		if typ == timeType {
			risky = append(risky, path)
			return
		}
		if typ.Implements(marshaler) || reflect.PointerTo(typ).Implements(marshaler) ||
			typ.Implements(textMarshaler) || reflect.PointerTo(typ).Implements(textMarshaler) {
			t.Fatalf("custom encoder requires review: %s", path)
		}
		switch typ.Kind() {
		case reflect.Pointer:
			inspect(typ.Elem(), path)
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				field := typ.Field(i)
				inspect(field.Type, path+"."+field.Name)
			}
		case reflect.Slice:
			if typ != reflect.TypeOf([]string(nil)) {
				t.Fatalf("new slice type requires review: %s", path)
			}
		case reflect.Float32, reflect.Float64:
			risky = append(risky, path)
		case reflect.String, reflect.Bool, reflect.Int, reflect.Int64:
		default:
			t.Fatalf("unsupported metadata type at %s: %s", path, typ)
		}
	}
	inspect(reflect.TypeOf(Job{}), "Job")
	inspect(reflect.TypeOf(AuthorImport{}), "AuthorImport")
	want := []string{
		"Job.CreatedAt", "Job.UpdatedAt", "Job.ArchiveUpdatedAt", "Job.DeletedAt",
		"Job.Subscription.ObservedAt", "Job.RequiredTier.TierPrice", "Job.RequiredTier.SinglePurchase",
		"AuthorImport.CreatedAt", "AuthorImport.UpdatedAt", "AuthorImport.LastCheckedAt", "AuthorImport.Subscription.ObservedAt",
	}
	slices.Sort(risky)
	slices.Sort(want)
	if !slices.Equal(risky, want) {
		t.Fatalf("metadata validator needs review: got %v want %v", risky, want)
	}
}

func TestSQLiteMetadataValidatedFallback(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	base := Job{CreatedAt: now, UpdatedAt: now, SharedWith: []string{"member"},
		Subscription: &CapturedSubscription{ObservedAt: now},
		RequiredTier: &boosty.PostRequirement{SinglePurchase: 12}}
	if !sqliteMetadataEqualValidated(base, cloneStorageJob(base)) {
		t.Fatal("ordinary equal record missed fast path")
	}
	invalid := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, mutate := range []func(*Job){
		func(j *Job) { j.CreatedAt = invalid },
		func(j *Job) { j.UpdatedAt = invalid },
		func(j *Job) { j.ArchiveUpdatedAt = invalid },
		func(j *Job) { j.DeletedAt = &invalid },
		func(j *Job) { j.Subscription.ObservedAt = invalid },
		func(j *Job) { j.RequiredTier.TierPrice = math.NaN() },
		func(j *Job) { j.RequiredTier.SinglePurchase = math.Inf(-1) },
	} {
		job := cloneStorageJob(base)
		mutate(&job)
		if sqliteMetadataEqualValidated(job, job) || sqliteShadowEqual(job, job) {
			t.Fatal("unserializable equal job accepted")
		}
	}
	for _, mutate := range []func(*AuthorImport){
		func(i *AuthorImport) { i.CreatedAt = invalid },
		func(i *AuthorImport) { i.UpdatedAt = invalid },
		func(i *AuthorImport) { i.LastCheckedAt = invalid },
		func(i *AuthorImport) { i.Subscription.ObservedAt = invalid },
	} {
		item := AuthorImport{CreatedAt: now, UpdatedAt: now, Subscription: &CapturedSubscription{ObservedAt: now}}
		mutate(&item)
		if sqliteMetadataEqualValidated(item, item) || sqliteShadowEqual(item, item) {
			t.Fatal("unserializable equal import accepted")
		}
	}
	local := base
	local.CreatedAt = now.In(time.FixedZone("offset", 3600))
	if sqliteMetadataEqualValidated(local, local) || !sqliteShadowEqual(local, local) {
		t.Fatal("non-UTC date did not use legacy fallback")
	}
	zero := Job{}
	empty := Job{SharedWith: []string{}}
	if sqliteMetadataEqualValidated(zero, empty) || !sqliteShadowEqual(zero, empty) {
		t.Fatal("omitempty equivalence did not use legacy fallback")
	}
}

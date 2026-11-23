package server

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSQLiteMetadataGrantDeltaPreservesUnchangedRows(t *testing.T) {
	root, _, _ := sqlPrimaryJournalFixture(t)
	meta, err := openSQLiteMetadata(filepath.Join(root, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer meta.close()
	ctx := context.Background()
	id := strings.Repeat("1", 24)
	job, err := meta.job(ctx, "first", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(job.SharedWith) == 0 {
		t.Fatal("fixture needs existing grant")
	}
	member := job.SharedWith[0]
	var created string
	if err := meta.db.QueryRow("SELECT created_at FROM archive_grants WHERE archive_id=? AND grantee_owner_id=?", id, member).Scan(&created); err != nil {
		t.Fatal(err)
	}
	next := job
	next.Title = "progress only"
	next.UpdatedAt = job.UpdatedAt.Add(time.Minute)
	if err := meta.apply(ctx, []sqliteArchiveChange{{Before: &job, After: next}}, nil); err != nil {
		t.Fatal(err)
	}
	expanded := next
	expanded.SharedWith = append(append([]string(nil), next.SharedWith...), "fresh-member")
	expanded.UpdatedAt = next.UpdatedAt.Add(time.Minute)
	if err := meta.apply(ctx, []sqliteArchiveChange{{Before: &next, After: expanded}}, nil); err != nil {
		t.Fatal(err)
	}
	removed := expanded
	removed.SharedWith = append([]string(nil), next.SharedWith...)
	removed.UpdatedAt = expanded.UpdatedAt.Add(time.Minute)
	if err := meta.apply(ctx, []sqliteArchiveChange{{Before: &expanded, After: removed}}, nil); err != nil {
		t.Fatal(err)
	}
	var actual string
	if err := meta.db.QueryRow("SELECT created_at FROM archive_grants WHERE archive_id=? AND grantee_owner_id=?", id, member).Scan(&actual); err != nil || actual != created {
		t.Fatal("retained grant timestamp changed", actual, err)
	}
	var count int
	if err := meta.db.QueryRow("SELECT COUNT(*) FROM archive_grants WHERE archive_id=? AND grantee_owner_id='fresh-member'", id).Scan(&count); err != nil || count != 0 {
		t.Fatal("revoked grant retained", count, err)
	}
	current, err := meta.job(ctx, "first", id)
	if err != nil || !sqliteShadowEqual(current, removed) {
		t.Fatal("projection drift after ACL delta", err)
	}
	if _, err := meta.db.Exec("INSERT INTO archive_grants(archive_id,grantee_owner_id,created_at) VALUES(?,?,?)", id, "forged-member", created); err != nil {
		t.Fatal(err)
	}
	bad := removed
	bad.Title = "must not commit"
	if err := meta.apply(ctx, []sqliteArchiveChange{{Before: &removed, After: bad}}, nil); err == nil {
		t.Fatal("forged ACL accepted")
	}
}

func TestSQLiteMetadataEqualityFastPath(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		equal bool
	}{
		{`{"a":1,"b":2}`, `{"a":1,"b":2}`, true},
		{`{"a":1,"b":2}`, `{"b":2,"a":1}`, true},
		{`{"a":1}`, `{"a":1.0}`, false},
		{`{"a":1}`, `{"a":2}`, false},
		{`{"a":1} garbage`, `{"a":1} garbage`, false},
		{`{} {}`, `{} {}`, false},
	} {
		if got := sqliteSameJSON([]byte(tc.a), []byte(tc.b)); got != tc.equal {
			t.Fatalf("equality mismatch %s %s: %v", tc.a, tc.b, got)
		}
	}
}

func benchmarkLegacySQLiteEquality(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil || !json.Valid(left) || !json.Valid(right) {
		return false
	}
	var l, r any
	da, db := json.NewDecoder(bytes.NewReader(left)), json.NewDecoder(bytes.NewReader(right))
	da.UseNumber()
	db.UseNumber()
	return da.Decode(&l) == nil && db.Decode(&r) == nil && reflect.DeepEqual(l, r)
}

func BenchmarkSQLiteMetadataEqual(b *testing.B) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	j := Job{ID: strings.Repeat("1", 24), OwnerID: "first", Author: "creator", PostID: "post", Title: "Archive metadata", State: "done", CreatedAt: now, UpdatedAt: now, ArchiveUpdatedAt: now, SharedWith: []string{"second", "third"}, IncludeVideos: true}
	for _, tc := range []struct {
		name  string
		equal func(any, any) bool
	}{{"legacy", benchmarkLegacySQLiteEquality}, {"optimized", sqliteShadowEqual}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if !tc.equal(j, j) {
					b.Fatal("equality failed")
				}
			}
		})
	}
}

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func typedDeltaFixture(n int) (map[string]Job, map[string]Job, metadataImage, error) {
	before := make(map[string]Job, n)
	after := make(map[string]Job, n)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%024x", i)
		job := Job{ID: id, OwnerID: "owner", Author: "creator", PostID: id,
			Title: "Archive", State: "done", CreatedAt: now, UpdatedAt: now,
			SharedWith: []string{"member"}}
		before[id], after[id] = job, job
	}
	id := fmt.Sprintf("%024x", n/2)
	job := after[id]
	job.Title = "Updated"
	after[id] = job
	delete(after, fmt.Sprintf("%024x", 0))
	added := job
	added.ID = fmt.Sprintf("%024x", n+1)
	added.PostID = added.ID
	after[added.ID] = added
	old, err := json.MarshalIndent(before, "", "  ")
	if err != nil {
		return nil, nil, metadataImage{}, err
	}
	next, err := json.MarshalIndent(after, "", "  ")
	if err != nil {
		return nil, nil, metadataImage{}, err
	}
	return before, after, metadataImage{Existed: true, Before: old, After: next,
		OldHash: metadataDigest(old), NewHash: metadataDigest(next)}, nil
}

func TestSQLPrimaryTypedDeltaExactRecovery(t *testing.T) {
	before, after, img, err := typedDeltaFixture(100)
	if err != nil {
		t.Fatal(err)
	}
	delta, ok := sqlitePrimaryTypedDelta(img, before, after)
	if !ok || len(delta.Rows) != 3 {
		t.Fatalf("expected 3 changed rows, got %d (%v)", len(delta.Rows), ok)
	}
	raw, err := json.Marshal(delta)
	if err != nil {
		t.Fatal(err)
	}
	var decoded sqlitePrimaryDeltaImage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, current := range [][]byte{img.Before, img.After} {
		got, err := sqlitePrimaryExpandDelta(current, decoded)
		if err != nil || !bytes.Equal(got.Before, img.Before) || !bytes.Equal(got.After, img.After) {
			t.Fatal("typed delta did not recover exact bytes", err)
		}
	}
}

func TestSQLPrimaryTypedDeltaRejectsUnknownFieldAndLegacyFormatting(t *testing.T) {
	before, after, img, err := typedDeltaFixture(5)
	if err != nil {
		t.Fatal(err)
	}
	// Inject a field absent from Job into an otherwise canonical unchanged
	// row. Typed equality must not hide its removal from the after image.
	var rows map[string]json.RawMessage
	if err := json.Unmarshal(img.Before, &rows); err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("%024x", 1)
	rows[id] = append(bytes.TrimRight(rows[id], " \r\n\t}"), []byte(",\"unknown\":true}")...)
	img.Before, err = json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	img.OldHash = metadataDigest(img.Before)
	if _, ok := sqlitePrimaryTypedDelta(img, before, after); ok {
		t.Fatal("typed delta accepted unknown-field mismatch")
	}
	// The previous generic path can express the unknown field removal.
	if _, ok := sqlitePrimaryMakeDelta(img); !ok {
		t.Fatal("generic delta fallback lost")
	}
	img.Before = append([]byte(" "), img.Before...)
	img.OldHash = metadataDigest(img.Before)
	if _, ok := sqlitePrimaryTypedDelta(img, before, after); ok {
		t.Fatal("typed delta accepted noncanonical legacy formatting")
	}
}

func BenchmarkSQLPrimaryTypedDelta(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		before, after, img, err := typedDeltaFixture(n)
		if err != nil {
			b.Fatal(err)
		}
		for _, mode := range []string{"generic", "typed"} {
			b.Run(fmt.Sprintf("%d/%s", n, mode), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					var delta sqlitePrimaryDeltaImage
					var ok bool
					if mode == "generic" {
						delta, ok = sqlitePrimaryMakeDelta(img)
					} else {
						delta, ok = sqlitePrimaryTypedDelta(img, before, after)
					}
					if !ok || len(delta.Rows) != 3 {
						b.Fatal("incorrect delta")
					}
				}
			})
		}
	}
}

package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

type failingMirrorReader struct{}

func (failingMirrorReader) Read([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestSQLiteMirrorStreamingExactGenerations(t *testing.T) {
	before := bytes.Repeat([]byte("a"), 64<<10)
	after := bytes.Clone(before)
	after[len(after)-1] = 'b'
	for _, tc := range []struct {
		name string
		raw  []byte
		old  []byte
		next []byte
		want bool
		bad  bool
	}{
		{"before", before, before, after, false, false},
		{"after", after, before, after, true, false},
		{"identical", before, before, before, true, false},
		{"short", before[:len(before)-1], before, after, false, true},
		{"long", append(bytes.Clone(after), 'x'), before, after, false, true},
		{"mismatch", bytes.Repeat([]byte("c"), len(before)), before, after, false, true},
		{"different-length", before[:123], before, before[:123], true, false},
		{"empty", nil, nil, before, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := matchSQLiteMirror(bytes.NewReader(tc.raw), tc.old, tc.next)
			if (err != nil) != tc.bad || (!tc.bad && got != tc.want) {
				t.Fatalf("match=%v err=%v", got, err)
			}
		})
	}
	if _, err := matchSQLiteMirror(failingMirrorReader{}, before, after); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("read error lost", err)
	}
}

func TestSQLiteUnchangedDeltaRetainsCanonicalRequirement(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{"{}", true},
		{"{\n  \"a\": 1\n}", true},
		{"{ \"a\": 1 }", false},
		{"null", false},
		{"broken", false},
	} {
		raw := []byte(tc.raw)
		img := metadataImage{Existed: true, Before: raw, After: raw, OldHash: metadataDigest(raw), NewHash: metadataDigest(raw)}
		delta, ok := sqlitePrimaryMakeDelta(img)
		if ok != tc.want {
			t.Fatalf("canonical result for %q: %v", tc.raw, ok)
		}
		if ok {
			if delta.Rows == nil || len(delta.Rows) != 0 {
				t.Fatal("unchanged image has row changes")
			}
			got, err := sqlitePrimaryExpandDelta(raw, delta)
			if err != nil || !bytes.Equal(got.Before, raw) || !bytes.Equal(got.After, raw) {
				t.Fatal("unchanged journal cannot recover", err)
			}
		}
	}
}

func BenchmarkSQLiteMirrorValidation(b *testing.B) {
	for _, size := range []int{1 << 20, 16 << 20, 64 << 20} {
		before := bytes.Repeat([]byte("a"), size)
		after := bytes.Clone(before)
		after[size-1] = 'b'
		for _, mode := range []string{"read-all", "streaming"} {
			b.Run(fmt.Sprintf("%dMiB/%s", size>>20, mode), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					reader := bytes.NewReader(after)
					if mode == "read-all" {
						raw, err := io.ReadAll(io.LimitReader(reader, int64(size)+1))
						if err != nil || !bytes.Equal(raw, after) {
							b.Fatal("read-all mismatch", err)
						}
					} else if current, err := matchSQLiteMirror(reader, before, after); err != nil || !current {
						b.Fatal("streaming mismatch", err)
					}
				}
			})
		}
	}
}

package server

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
)

func TestGuardedCopyBufferBoundAndClear(t *testing.T) {
	var buffers [guardedCopyPoolCapacity + 2]*guardedCopyBuffer
	seen := make(map[*guardedCopyBuffer]bool)
	for i := range buffers {
		buf := acquireGuardedCopyBuffer()
		if seen[buf] {
			t.Fatal("active buffer reused")
		}
		seen[buf] = true
		buffers[i] = buf
		for j := range buf {
			buf[j] = 0xa5
		}
	}
	for _, buf := range buffers {
		releaseGuardedCopyBuffer(buf)
	}
	if len(guardedCopyBuffers) != guardedCopyPoolCapacity {
		t.Fatal("idle pool exceeds capacity or lost buffers")
	}
	for _, buf := range buffers {
		for _, value := range buf {
			if value != 0 {
				t.Fatal("previous payload retained")
			}
		}
	}
}

func TestGuardedCopyConcurrentBufferIsolation(t *testing.T) {
	var wg sync.WaitGroup
	for i := 1; i <= 8; i++ {
		wg.Add(1)
		go func(value byte) {
			defer wg.Done()
			payload := bytes.Repeat([]byte{value}, 2*guardedCopyChunkBytes+13)
			var out bytes.Buffer
			n, err := guardedCopy(context.Background(), &out, bytes.NewReader(payload))
			if err != nil || n != int64(len(payload)) || !bytes.Equal(out.Bytes(), payload) {
				t.Errorf("concurrent copy %d corrupted: %d %v", value, n, err)
			}
		}(byte(i))
	}
	wg.Wait()
}

func BenchmarkGuardedCopyBuffer(b *testing.B) {
	payload := bytes.Repeat([]byte("x"), 64<<10)
	ctx := context.Background()
	b.Run("fresh", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(payload)))
		for i := 0; i < b.N; i++ {
			if _, err := io.CopyBuffer(guardedWriter{ctx: ctx, out: io.Discard}, guardedCopyReader{bytes.NewReader(payload)}, make([]byte, guardedCopyChunkBytes)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("pooled", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(payload)))
		for i := 0; i < b.N; i++ {
			if _, err := guardedCopy(ctx, io.Discard, bytes.NewReader(payload)); err != nil {
				b.Fatal(err)
			}
		}
	})
}

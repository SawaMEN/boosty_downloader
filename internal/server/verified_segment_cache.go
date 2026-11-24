package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const verifiedSegmentCacheMaxBytes int64 = 256 << 20
const verifiedSegmentCacheMaxEntries = 512

type verifiedSegmentCacheKey struct{}

type cachedVideoSegment struct {
	name   string
	etag   string
	digest string
	size   int64
}

type verifiedSegmentCache struct {
	dir     string
	entries map[string]cachedVideoSegment
	bytes   int64
	serial  int
}

// This cache belongs to exactly one media download, not a user, server or
// persistent archive. URL keys (including tokens) stay in memory. A failure
// to create the optional cache must not prevent normal video archiving.
func withVerifiedSegmentCache(ctx context.Context, dest string) (context.Context, func()) {
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".boosty-segment-cache-")
	if err != nil {
		return ctx, func() {}
	}
	cache := &verifiedSegmentCache{dir: dir, entries: make(map[string]cachedVideoSegment)}
	return context.WithValue(ctx, verifiedSegmentCacheKey{}, cache), func() {
		if os.RemoveAll(dir) == nil {
			releaseStorageBudget(ctx, cache.bytes)
			cache.bytes = 0
			clear(cache.entries)
		}
	}
}

func strongSegmentETag(h http.Header) string {
	tag := strings.TrimSpace(h.Get("ETag"))
	if len(tag) < 2 || tag[0] != '"' || tag[len(tag)-1] != '"' || strings.ContainsAny(tag[1:len(tag)-1], "\"\r\n") {
		return ""
	}
	return tag
}

func rememberCachedSegment(ctx context.Context, raw, source string, headers http.Header, n int64) {
	cache, _ := ctx.Value(verifiedSegmentCacheKey{}).(*verifiedSegmentCache)
	tag := strongSegmentETag(headers)
	if cache == nil || tag == "" || n <= 0 || n > verifiedSegmentCacheMaxBytes-cache.bytes ||
		len(cache.entries) >= verifiedSegmentCacheMaxEntries {
		return
	}
	if _, exists := cache.entries[raw]; exists {
		return
	}
	in, err := openArchiveRegular(filepath.Dir(source), filepath.Base(source))
	if err != nil {
		return
	}
	defer in.Close()
	cache.serial++
	name := fmt.Sprintf("segment-%06d", cache.serial)
	path := filepath.Join(cache.dir, name)
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	hash := sha256.New()
	written, copyErr := guardedCopy(ctx, io.MultiWriter(out, hash), io.LimitReader(in, n+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || written != n {
		if os.Remove(path) == nil {
			releaseStorageBudget(ctx, written)
		} else {
			cache.bytes += written
		}
		return
	}
	cache.bytes += n
	cache.entries[raw] = cachedVideoSegment{name: name, etag: tag, digest: hex.EncodeToString(hash.Sum(nil)), size: n}
}

// A strong validator is scoped to its exact resource URL. Do not strip
// signatures, normalize paths or compare ETags across different URLs.
// A one-byte conditional range probe proves the same complete resource is
// still available. Reject a missing/weak/changed validator, 200 fallback,
// unexpected range or content encoding, then use the normal full download.
func reuseCachedSegment(ctx context.Context, fetch mediaFetcher, raw, path string, budget int64) (int64, bool) {
	cache, _ := ctx.Value(verifiedSegmentCacheKey{}).(*verifiedSegmentCache)
	if cache == nil {
		return 0, false
	}
	item, ok := cache.entries[raw]
	if !ok || item.size > budget || item.size > maxHLSSegmentBytes || ctx.Err() != nil {
		return 0, false
	}
	res, err := fetch(ctx, raw, "bytes=0-0", item.etag)
	if err != nil {
		return 0, false
	}
	defer res.Body.Close()
	start, end, total, rangeErr := parseByteRange(res.Header.Get("Content-Range"))
	encoding := strings.TrimSpace(res.Header.Get("Content-Encoding"))
	if res.StatusCode != http.StatusPartialContent || rangeErr != nil || start != 0 || end != 0 ||
		total != item.size || strongSegmentETag(res.Header) != item.etag ||
		(res.ContentLength >= 0 && res.ContentLength != 1) || (encoding != "" && !strings.EqualFold(encoding, "identity")) {
		return 0, false
	}
	probe, err := io.ReadAll(io.LimitReader(res.Body, 2))
	if err != nil || len(probe) != 1 {
		return 0, false
	}
	in, err := openArchiveRegular(cache.dir, item.name)
	if err != nil {
		return 0, false
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || info.Size() != item.size {
		return 0, false
	}
	var first [1]byte
	if _, err := io.ReadFull(in, first[:]); err != nil || first[0] != probe[0] {
		return 0, false
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return 0, false
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, false
	}
	hash := sha256.New()
	n, copyErr := guardedCopy(ctx, io.MultiWriter(out, hash), io.LimitReader(in, item.size+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || n != item.size || hex.EncodeToString(hash.Sum(nil)) != item.digest {
		if os.Remove(path) == nil {
			releaseStorageBudget(ctx, n)
		}
		return 0, false
	}
	return n, true
}

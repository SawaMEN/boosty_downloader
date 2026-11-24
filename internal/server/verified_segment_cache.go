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

type videoSegmentKey struct {
	url    string
	offset int64
	length int64 // zero identifies a complete HTTP resource
}

type cachedVideoSegment struct {
	name     string
	etag     string
	digest   string
	finalURL string
	size     int64
	total    int64
}

type verifiedSegmentCache struct {
	parent  string
	dir     string
	entries map[videoSegmentKey]cachedVideoSegment
	bytes   int64
	serial  int
	closed  bool
}

// This cache belongs to exactly one media download, not a user, server or
// persistent archive. URL keys (including tokens) stay in memory. A failure
// to create the optional cache must not prevent normal video archiving.
func withVerifiedSegmentCache(ctx context.Context, dest string) (context.Context, func()) {
	cache := &verifiedSegmentCache{parent: filepath.Dir(dest)}
	return context.WithValue(ctx, verifiedSegmentCacheKey{}, cache), func() {
		cache.closed = true
		if cache.dir != "" && os.RemoveAll(cache.dir) == nil {
			releaseStorageBudget(withoutMediaStageBudget(ctx), cache.bytes)
			cache.bytes = 0
			clear(cache.entries)
		}
	}
}

func (c *verifiedSegmentCache) ensureDir() bool {
	if c.closed {
		return false
	}
	if c.dir != "" {
		return true
	}
	dir, err := os.MkdirTemp(c.parent, ".boosty-segment-cache-")
	if err != nil {
		// Caching is optional; avoid retrying mkdir for every segment.
		c.closed = true
		return false
	}
	c.dir = dir
	c.entries = make(map[videoSegmentKey]cachedVideoSegment)
	return true
}

// Remove only this cache's own entry and credit only charged bytes after
// the pathname has been removed. Failed removals remain charged until
// directory cleanup; the stale entry is never used again.
func (c *verifiedSegmentCache) invalidate(ctx context.Context, key videoSegmentKey, item cachedVideoSegment) {
	delete(c.entries, key)
	if err := os.Remove(filepath.Join(c.dir, item.name)); err == nil || os.IsNotExist(err) {
		c.bytes -= item.size
		releaseStorageBudget(withoutMediaStageBudget(ctx), item.size)
	}
}

func strongSegmentETag(h http.Header) string {
	tag := strings.TrimSpace(h.Get("ETag"))
	if len(tag) < 2 || tag[0] != '"' || tag[len(tag)-1] != '"' || strings.ContainsAny(tag[1:len(tag)-1], "\"\r\n") {
		return ""
	}
	return tag
}

func segmentIdentityEncoding(h http.Header) bool {
	encoding := strings.TrimSpace(h.Get("Content-Encoding"))
	return encoding == "" || strings.EqualFold(encoding, "identity")
}

func segmentResponseURL(res *http.Response, raw string) string {
	if res.Request != nil && res.Request.URL != nil {
		return res.Request.URL.String()
	}
	return raw
}

func rememberCachedSegment(ctx context.Context, key videoSegmentKey, source string, res *http.Response, n, total int64) {
	cache, _ := ctx.Value(verifiedSegmentCacheKey{}).(*verifiedSegmentCache)
	tag := strongSegmentETag(res.Header)
	if cache == nil || cache.closed || tag == "" || !segmentIdentityEncoding(res.Header) || n <= 0 ||
		n > verifiedSegmentCacheMaxBytes-cache.bytes ||
		len(cache.entries) >= verifiedSegmentCacheMaxEntries {
		return
	}
	if _, exists := cache.entries[key]; exists {
		return
	}
	// The cache directory outlives an individual remux attempt. Its
	// writes must not be credited when that attempt's directory is removed.
	ctx = withoutMediaStageBudget(ctx)
	in, err := openArchiveRegular(filepath.Dir(source), filepath.Base(source))
	if err != nil {
		return
	}
	defer in.Close()
	if !cache.ensureDir() {
		return
	}
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
	cache.entries[key] = cachedVideoSegment{
		name: name, etag: tag, digest: hex.EncodeToString(hash.Sum(nil)),
		finalURL: segmentResponseURL(res, key.url), size: n, total: total,
	}
}

// A strong validator is scoped to its exact resource URL. Do not strip
// signatures, normalize paths or compare ETags across different URLs.
// A one-byte conditional range probe proves the same complete resource is
// still available. Reject a missing/weak/changed validator, 200 fallback,
// unexpected range or content encoding, then use the normal full download.
func reuseCachedSegment(ctx context.Context, fetch mediaFetcher, key videoSegmentKey, path string, budget int64) (int64, bool) {
	cache, _ := ctx.Value(verifiedSegmentCacheKey{}).(*verifiedSegmentCache)
	if cache == nil || cache.closed {
		return 0, false
	}
	item, ok := cache.entries[key]
	if !ok || item.size > budget || item.size > maxHLSSegmentBytes || ctx.Err() != nil {
		return 0, false
	}
	res, err := fetch(ctx, key.url, fmt.Sprintf("bytes=%d-%d", key.offset, key.offset), item.etag)
	if err != nil {
		return 0, false
	}
	defer res.Body.Close()
	start, end, total, rangeErr := parseByteRange(res.Header.Get("Content-Range"))
	tag := strongSegmentETag(res.Header)
	// A changed strong validator on the same resource is definitive.
	// Drop the old entry so a subsequent full download can cache the new one.
	if tag != "" && tag != item.etag && segmentResponseURL(res, key.url) == item.finalURL {
		cache.invalidate(ctx, key, item)
		return 0, false
	}
	if res.StatusCode != http.StatusPartialContent || rangeErr != nil || start != key.offset || end != key.offset ||
		total != item.total || tag != item.etag ||
		segmentResponseURL(res, key.url) != item.finalURL ||
		(res.ContentLength >= 0 && res.ContentLength != 1) || !segmentIdentityEncoding(res.Header) {
		return 0, false
	}
	probe, err := io.ReadAll(io.LimitReader(res.Body, 2))
	if err != nil || len(probe) != 1 {
		return 0, false
	}
	in, err := openArchiveRegular(cache.dir, item.name)
	if err != nil {
		cache.invalidate(ctx, key, item)
		return 0, false
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || info.Size() != item.size {
		cache.invalidate(ctx, key, item)
		return 0, false
	}
	var first [1]byte
	if _, err := io.ReadFull(in, first[:]); err != nil || first[0] != probe[0] {
		cache.invalidate(ctx, key, item)
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
	damaged := copyErr == nil && (n != item.size || hex.EncodeToString(hash.Sum(nil)) != item.digest)
	if copyErr != nil || closeErr != nil || damaged {
		if os.Remove(path) == nil {
			releaseStorageBudget(ctx, n)
		}
		if damaged {
			cache.invalidate(ctx, key, item)
		}
		return 0, false
	}
	return n, true
}

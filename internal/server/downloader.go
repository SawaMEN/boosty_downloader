package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// All media is written to a private .part file first. A partial file is resumed
// only when its server-issued validator is available for If-Range; otherwise it
// is restarted so a replaced media object cannot silently corrupt an archive.
const maxArchiveMediaSize int64 = 10 << 30

type mediaFetcher func(context.Context, string, string, string) (*http.Response, error)

type partialMedia struct {
	Validator string `json:"validator"`
	Total     int64  `json:"total"`
}

func usableValidator(h http.Header) string {
	tag := strings.TrimSpace(h.Get("ETag"))
	if strings.HasPrefix(tag, "\"") && strings.HasSuffix(tag, "\"") && len(tag) > 2 && !strings.ContainsAny(tag, "\r\n") {
		return tag // Weak ETags are not suitable for byte-for-byte resume.
	}
	modified := h.Get("Last-Modified")
	if modified != "" && !strings.ContainsAny(modified, "\r\n") {
		if _, err := http.ParseTime(modified); err == nil {
			return modified
		}
	}
	return ""
}

func parseByteRange(s string) (start, end, total int64, err error) {
	if !strings.HasPrefix(s, "bytes ") {
		return 0, 0, 0, errors.New("CDN не передал Content-Range")
	}
	bounds, full, ok := strings.Cut(strings.TrimPrefix(s, "bytes "), "/")
	if !ok || full == "*" {
		return 0, 0, 0, errors.New("неизвестен полный размер Content-Range")
	}
	from, to, ok := strings.Cut(bounds, "-")
	if !ok {
		return 0, 0, 0, errors.New("неверный Content-Range")
	}
	start, err = strconv.ParseInt(from, 10, 64)
	if err != nil {
		return
	}
	end, err = strconv.ParseInt(to, 10, 64)
	if err != nil {
		return
	}
	total, err = strconv.ParseInt(full, 10, 64)
	if err != nil {
		return
	}
	if start < 0 || end < start || total <= end || total > maxArchiveMediaSize {
		err = errors.New("недопустимые границы Content-Range")
	}
	return
}

func fileDigest(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxArchiveMediaSize+1))
	if err != nil {
		return 0, "", err
	}
	if n > maxArchiveMediaSize {
		return 0, "", errors.New("файл превышает лимит размера")
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func verifyAsset(path string, asset ArchiveAsset) (bool, error) {
	if asset.SHA256 == "" || len(asset.SHA256) != 64 || asset.Size < 0 {
		return false, nil // Older archives have no digest and must be revalidated by downloading.
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != asset.Size {
		return false, nil
	}
	size, digest, err := fileDigest(path)
	return err == nil && size == asset.Size && strings.EqualFold(digest, asset.SHA256), err
}

func downloadFile(ctx context.Context, clientMedia mediaFetcher, raw, dest string, expected int64) (int64, string, error) {
	if raw == "" {
		return 0, "", errors.New("медиа без URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return 0, "", errors.New("неверная ссылка на медиа")
	}
	remotePath := strings.ToLower(u.Path)
	if strings.HasSuffix(remotePath, ".m3u8") || strings.HasSuffix(remotePath, ".mpd") {
		return 0, "", errors.New("HLS/DASH требует загрузки плейлиста и всех сегментов")
	}
	if expected > maxArchiveMediaSize {
		return 0, "", errors.New("медиа превышает предел 10 ГиБ")
	}
	tmp, sidecar := dest+".part", dest+".part.meta"
	var state partialMedia
	var existing int64
	if info, err := os.Lstat(tmp); err == nil {
		if !info.Mode().IsRegular() {
			return 0, "", errors.New("необычный тип частичного файла")
		}
		existing = info.Size()
		b, metaErr := os.ReadFile(sidecar)
		if metaErr != nil || json.Unmarshal(b, &state) != nil || state.Validator == "" || state.Total <= existing || existing < 0 || existing >= maxArchiveMediaSize {
			existing = 0
			if err := os.Remove(tmp); err != nil {
				return 0, "", err
			}
			_ = os.Remove(sidecar)
		}
	} else if !os.IsNotExist(err) {
		return 0, "", err
	}

	rangeHeader, ifRange := "", ""
	if existing > 0 {
		rangeHeader = fmt.Sprintf("bytes=%d-", existing)
		ifRange = state.Validator
	}
	res, err := clientMedia(ctx, raw, rangeHeader, ifRange)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		return 0, "", fmt.Errorf("CDN HTTP %d", res.StatusCode)
	}
	// HLS and DASH playlists are NOT MP4 files. They require a separate
	// segment downloader/FFmpeg pipeline and are never archived as .mp4.
	ct := strings.ToLower(strings.Split(res.Header.Get("Content-Type"), ";")[0])
	if strings.Contains(ct, "mpegurl") || ct == "application/dash+xml" || ct == "application/vnd.ms-sstr+xml" {
		return 0, "", errors.New("HLS/DASH требует отдельного загрузчика сегментов")
	}
	if strings.HasSuffix(strings.ToLower(dest), ".mp4") && (ct == "text/html" || ct == "application/json" || ct == "text/plain") {
		return 0, "", errors.New("CDN вернул страницу вместо видео")
	}

	total := int64(-1)
	if res.StatusCode == http.StatusPartialContent {
		var start, end int64
		start, end, total, err = parseByteRange(res.Header.Get("Content-Range"))
		if err != nil {
			return 0, "", err
		}
		if start != existing {
			return 0, "", errors.New("CDN вернул неправильное начало диапазона")
		}
		if res.ContentLength >= 0 && res.ContentLength != end-start+1 {
			return 0, "", errors.New("CDN вернул неверный Content-Length диапазона")
		}
		if existing > 0 && state.Total > 0 && state.Total != total {
			return 0, "", errors.New("медиа изменилось между запросами")
		}
		if existing > 0 {
			if validator := usableValidator(res.Header); validator != "" && validator != state.Validator {
				return 0, "", errors.New("изменился ETag/Last-Modified медиа")
			}
		}
	} else {
		// An ignored If-Range means the server sent a fresh complete object.
		existing = 0
		if res.ContentLength >= 0 {
			total = res.ContentLength
		}
	}
	if total > maxArchiveMediaSize {
		return 0, "", errors.New("медиа превышает предел 10 ГиБ")
	}
	if expected > 0 && total >= 0 && expected != total {
		return 0, "", errors.New("Boosty и CDN сообщили разные размеры медиа")
	}
	if existing == 0 {
		// Never reuse the validator of a different HTTP representation.
		_ = os.Remove(sidecar)
		state = partialMedia{Validator: usableValidator(res.Header), Total: total}
		if state.Validator != "" && state.Total > 0 {
			if err := writeJSON(sidecar, state); err != nil {
				return 0, "", err
			}
		}
	}
	flag := os.O_CREATE | os.O_WRONLY
	if existing == 0 {
		flag |= os.O_TRUNC
	} else {
		flag |= os.O_APPEND
	}
	f, err := os.OpenFile(tmp, flag, 0600)
	if err != nil {
		return 0, "", err
	}
	// Read one additional byte to reject a response larger than the limit.
	n, copyErr := io.Copy(f, io.LimitReader(res.Body, maxArchiveMediaSize-existing+1))
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil {
		return 0, "", copyErr
	}
	if syncErr != nil {
		return 0, "", syncErr
	}
	if closeErr != nil {
		return 0, "", closeErr
	}
	size := existing + n
	if size > maxArchiveMediaSize {
		_ = os.Remove(tmp)
		_ = os.Remove(sidecar)
		return 0, "", errors.New("медиа превышает предел 10 ГиБ")
	}
	if res.ContentLength >= 0 && n != res.ContentLength {
		return 0, "", errors.New("CDN передал неполный файл")
	}
	if total >= 0 && size != total {
		return 0, "", errors.New("несовпадение полного размера файла")
	}
	if expected > 0 && size != expected {
		return 0, "", errors.New("несовпадение размера с метаданными Boosty")
	}
	hashedSize, digest, err := fileDigest(tmp)
	if err != nil || hashedSize != size {
		return 0, "", errors.New("ошибка контрольной суммы медиа")
	}
	if err := os.Rename(tmp, dest); err != nil {
		return 0, "", err
	}
	_ = os.Remove(sidecar)
	// Synchronize the directory entry after the atomic final rename.
	if dir, err := os.Open(filepath.Dir(dest)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return size, digest, nil
}

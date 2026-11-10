package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// Byte-range offsets are explicit or inherit from the immediately preceding
// range of the *same* URI. This prevents downloading unrelated bytes when a
// playlist omits the optional @offset for a different media resource.
func parseHLSRangeSpec(spec string, previousEnd int64, implicitAllowed bool) (offset, length int64, err error) {
	fields := strings.Split(strings.TrimSpace(spec), "@")
	if len(fields) < 1 || len(fields) > 2 {
		return 0, 0, errors.New("HLS: некорректный BYTERANGE")
	}
	length, err = strconv.ParseInt(fields[0], 10, 64)
	if err != nil || length <= 0 || length > maxHLSSegmentBytes {
		return 0, 0, errors.New("HLS: недопустимая длина BYTERANGE")
	}
	if len(fields) == 2 {
		offset, err = strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, 0, errors.New("HLS: неверное смещение BYTERANGE")
		}
	} else {
		if !implicitAllowed {
			return 0, 0, errors.New("HLS: BYTERANGE без смещения и предыдущего сегмента")
		}
		offset = previousEnd
	}
	if offset < 0 || offset > maxArchiveMediaSize-length {
		return 0, 0, errors.New("HLS: BYTERANGE за пределами допустимого размера")
	}
	return offset, length, nil
}

func fetchHLSRangeSegment(ctx context.Context, fetch mediaFetcher, raw, path string, budget, offset, length int64) (int64, error) {
	if length == 0 {
		return fetchHLSSegment(ctx, fetch, raw, path, budget)
	}
	if length <= 0 || offset < 0 || length > budget || length > maxHLSSegmentBytes || offset > maxArchiveMediaSize-length {
		return 0, errors.New("HLS: диапазон превышает лимит")
	}
	end := offset + length - 1
	res, err := fetch(ctx, raw, fmt.Sprintf("bytes=%d-%d", offset, end), "")
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusPartialContent {
		return 0, errors.New("HLS: CDN не выполнил запрос BYTERANGE (нужен HTTP 206)")
	}
	start, last, _, err := parseByteRange(res.Header.Get("Content-Range"))
	if err != nil || start != offset || last != end {
		return 0, errors.New("HLS: неверный Content-Range ответа CDN")
	}
	if res.ContentLength >= 0 && res.ContentLength != length {
		return 0, errors.New("HLS: длина ответа CDN не совпала с BYTERANGE")
	}
	ct := strings.ToLower(res.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/json") || strings.Contains(ct, "mpegurl") || strings.Contains(ct, "dash+xml") {
		return 0, errors.New("HLS: CDN вернул не сегмент")
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, io.LimitReader(res.Body, length+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || n != length {
		_ = os.Remove(path)
		if copyErr != nil {
			return 0, copyErr
		}
		if closeErr != nil {
			return 0, closeErr
		}
		return 0, errors.New("HLS: неполный BYTERANGE сегмент")
	}
	return n, nil
}

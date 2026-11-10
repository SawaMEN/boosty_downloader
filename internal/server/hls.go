package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// HLS is resolved through the Boosty HTTP transport, never through FFmpeg's
// network protocols. FFmpeg sees a newly generated manifest containing only
// filenames created by the archive worker in its private staging directory.
const (
	maxHLSManifestBytes       = 2 << 20
	maxHLSSegmentBytes  int64 = 512 << 20
	maxHLSSegments            = 12000
	maxHLSDepth               = 3
)

type hlsPart struct {
	url           string
	initURL       string
	duration      float64
	discontinuity bool
	byteOffset    int64
	byteLength    int64
	initOffset    int64
	initLength    int64
}

type hlsList struct {
	variants []hlsVariant
	parts    []hlsPart
	vod      bool
}

type hlsVariant struct {
	url       string
	bandwidth int64
}

func hlsResourceURL(base, ref string) (string, error) {
	parent, err := url.Parse(base)
	if err != nil || parent.Scheme != "https" || parent.Hostname() == "" || parent.User != nil {
		return "", errors.New("неверный HTTPS-адрес плейлиста")
	}
	rel, err := url.Parse(strings.TrimSpace(ref))
	if err != nil || rel.IsAbs() && rel.Scheme != "https" || rel.User != nil || rel.Fragment != "" {
		return "", errors.New("небезопасный адрес сегмента HLS")
	}
	resolved := parent.ResolveReference(rel)
	if resolved.Scheme != "https" || resolved.Hostname() == "" || resolved.User != nil || resolved.Port() != "" && resolved.Port() != "443" {
		return "", errors.New("недопустимый адрес сегмента HLS")
	}
	// Signed query parameters are often attached to the parent manifest and
	// omitted from its relative children. Never disclose them to another host.
	if rel.RawQuery == "" && resolved.Hostname() == parent.Hostname() && resolved.Port() == parent.Port() {
		resolved.RawQuery = parent.RawQuery
	}
	return resolved.String(), nil
}

func hlsQuotedURI(tag string) (string, error) {
	const start = `URI="`
	i := strings.Index(tag, start)
	if i < 0 {
		return "", errors.New("HLS: отсутствует URI")
	}
	value := tag[i+len(start):]
	j := strings.IndexByte(value, '"')
	if j < 0 || j == 0 {
		return "", errors.New("HLS: неверный URI")
	}
	return value[:j], nil
}

func parseHLS(raw []byte, parent string) (hlsList, error) {
	var result hlsList
	if len(raw) == 0 || len(raw) > maxHLSManifestBytes {
		return result, errors.New("HLS: недопустимый размер плейлиста")
	}
	scan := bufio.NewScanner(strings.NewReader(strings.TrimPrefix(string(raw), "\ufeff")))
	scan.Buffer(make([]byte, 4096), maxHLSManifestBytes+1024)
	first := true
	pendingVariant := false
	pendingDuration := -1.0
	pendingDiscontinuity := false
	pendingRange := ""
	initURL := ""
	var initOffset, initLength int64
	var previousEnd int64
	previousURL := ""
	var bandwidth int64
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if first {
			if line != "#EXTM3U" {
				return result, errors.New("HLS: отсутствует заголовок #EXTM3U")
			}
			first = false
			continue
		}
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			attrs, e := hlsMasterAttributeList(strings.TrimPrefix(line, "#EXT-X-KEY:"))
			if e != nil || attrs["METHOD"] != "NONE" {
				return result, errors.New("HLS: зашифрованный поток не поддерживается")
			}
		case strings.HasPrefix(line, "#EXT-X-SESSION-KEY:"):
			return result, errors.New("HLS: ключ шифрования в master-плейлисте")
		case strings.HasPrefix(line, "#EXT-X-PART:") || strings.HasPrefix(line, "#EXT-X-SKIP:") || strings.HasPrefix(line, "#EXT-X-PRELOAD-HINT:") || strings.HasPrefix(line, "#EXT-X-I-FRAMES-ONLY"):
			return result, errors.New("HLS: LL-HLS/частичные сегменты не поддерживаются")
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			if pendingRange != "" {
				return result, errors.New("HLS: повтор BYTERANGE без сегмента")
			}
			pendingRange = strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-BYTERANGE:"))
			if pendingRange == "" {
				return result, errors.New("HLS: пустой BYTERANGE")
			}
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			if pendingVariant || len(result.variants) >= 100 {
				return result, errors.New("HLS: слишком много вариантов")
			}
			attrs, e := hlsMasterAttributeList(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			if e != nil {
				return result, e
			}
			if attrs["AUDIO"] != "" || attrs["VIDEO"] != "" {
				return result, errors.New("HLS: аудио/видео группы обрабатываются отдельно")
			}
			bandwidth, _ = strconv.ParseInt(attrs["BANDWIDTH"], 10, 64)
			pendingVariant = true
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			attrs, e := hlsMasterAttributeList(strings.TrimPrefix(line, "#EXT-X-MAP:"))
			if e != nil {
				return result, e
			}
			if attrs["URI"] == "" {
				return result, errors.New("HLS: EXT-X-MAP без URI")
			}
			initURL, e = hlsResourceURL(parent, attrs["URI"])
			if e != nil {
				return result, e
			}
			initOffset, initLength = 0, 0
			if spec := attrs["BYTERANGE"]; spec != "" {
				initOffset, initLength, e = parseHLSRangeSpec(spec, 0, false)
				if e != nil {
					return result, e
				}
			}
		case strings.HasPrefix(line, "#EXTINF:"):
			value, _, _ := strings.Cut(strings.TrimPrefix(line, "#EXTINF:"), ",")
			n, e := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if e != nil || n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) || n > 36000 {
				return result, errors.New("HLS: некорректная длительность сегмента")
			}
			pendingDuration = n
		case line == "#EXT-X-ENDLIST":
			result.vod = true
		case line == "#EXT-X-DISCONTINUITY":
			pendingDiscontinuity = true
		case strings.HasPrefix(line, "#"):
			// Timing and metadata tags are never forwarded to local FFmpeg.
		default:
			resolved, e := hlsResourceURL(parent, line)
			if e != nil {
				return result, e
			}
			if pendingVariant {
				result.variants = append(result.variants, hlsVariant{url: resolved, bandwidth: bandwidth})
				pendingVariant = false
			} else {
				if pendingDuration < 0 {
					return result, errors.New("HLS: у сегмента отсутствует EXTINF")
				}
				if len(result.parts) >= maxHLSSegments {
					return result, errors.New("HLS: слишком много сегментов")
				}
				part := hlsPart{url: resolved, initURL: initURL, initOffset: initOffset, initLength: initLength, duration: pendingDuration, discontinuity: pendingDiscontinuity}
				if pendingRange != "" {
					part.byteOffset, part.byteLength, e = parseHLSRangeSpec(pendingRange, previousEnd, previousURL == resolved)
					if e != nil {
						return result, e
					}
					previousEnd = part.byteOffset + part.byteLength
					previousURL = resolved
				} else {
					previousEnd = 0
					previousURL = ""
				}
				result.parts = append(result.parts, part)
				pendingRange = ""
				pendingDuration = -1
				pendingDiscontinuity = false
			}
		}
	}
	if err := scan.Err(); err != nil {
		return result, errors.New("HLS: слишком длинная строка плейлиста")
	}
	if first || pendingVariant || pendingDuration >= 0 || pendingRange != "" {
		return result, errors.New("HLS: незавершённый плейлист")
	}
	if len(result.variants) > 0 && len(result.parts) > 0 {
		return result, errors.New("HLS: смешаны master и media плейлисты")
	}
	if len(result.variants) == 0 && (len(result.parts) == 0 || !result.vod) {
		return result, errors.New("HLS: запись live-потоков не поддерживается; нужен конечный VOD")
	}
	return result, nil
}

func fetchHLSBytes(ctx context.Context, fetch mediaFetcher, raw string, max int64) ([]byte, error) {
	res, err := fetch(ctx, raw, "", "")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HLS CDN вернул HTTP %d", res.StatusCode)
	}
	if res.ContentLength > max {
		return nil, errors.New("HLS: превышен размер сегмента или плейлиста")
	}
	contentType := strings.ToLower(res.Header.Get("Content-Type"))
	if strings.HasPrefix(contentType, "text/html") || strings.HasPrefix(contentType, "application/json") {
		return nil, errors.New("HLS CDN вернул страницу вместо медиа")
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("HLS: превышен размер сегмента или плейлиста")
	}
	if res.ContentLength >= 0 && int64(len(b)) != res.ContentLength {
		return nil, errors.New("HLS: неполный ответ CDN")
	}
	return b, nil
}

// Segments are streamed to disk; even a 512 MiB segment never occupies 512 MiB
// of Go heap. Return its verified byte length for the total archive limit.
func fetchHLSSegment(ctx context.Context, fetch mediaFetcher, raw, path string, budget int64) (int64, error) {
	if budget <= 0 {
		return 0, errors.New("HLS: превышен лимит 10 ГиБ")
	}
	limit := budget
	if limit > maxHLSSegmentBytes {
		limit = maxHLSSegmentBytes
	}
	res, err := fetch(ctx, raw, "", "")
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HLS CDN вернул HTTP %d", res.StatusCode)
	}
	ct := strings.ToLower(res.Header.Get("Content-Type"))
	if strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/json") || strings.Contains(ct, "mpegurl") || strings.Contains(ct, "dash+xml") {
		return 0, errors.New("HLS CDN вернул не сегмент")
	}
	if res.ContentLength > limit {
		return 0, errors.New("HLS: превышен лимит размера сегмента")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(file, io.LimitReader(res.Body, limit+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || n == 0 || n > limit || res.ContentLength >= 0 && n != res.ContentLength {
		_ = os.Remove(path)
		if copyErr != nil {
			return 0, copyErr
		}
		if closeErr != nil {
			return 0, closeErr
		}
		return 0, errors.New("HLS: сегмент пустой, неполный или слишком большой")
	}
	return n, nil
}

func resolveHLS(ctx context.Context, fetch mediaFetcher, raw string) ([]hlsPart, error) {
	for depth := 0; depth < maxHLSDepth; depth++ {
		b, e := fetchHLSBytes(ctx, fetch, raw, maxHLSManifestBytes)
		if e != nil {
			return nil, e
		}
		playlist, e := parseHLS(b, raw)
		if e != nil {
			return nil, e
		}
		if len(playlist.variants) == 0 {
			return playlist.parts, nil
		}
		best := playlist.variants[0]
		for _, v := range playlist.variants[1:] {
			if v.bandwidth > best.bandwidth {
				best = v
			}
		}
		raw = best.url
	}
	return nil, errors.New("HLS: слишком много вложенных плейлистов")
}

// archiveHLS stages finite, unencrypted video and optional AUDIO-group
// renditions locally. FFmpeg cannot make network requests and the existing
// destination is not replaced until an MP4 has passed the digest check.
func archiveHLS(ctx context.Context, fetch mediaFetcher, raw, dest string) (int64, string, error) {
	video, audio, err := resolveHLSWithAudio(ctx, fetch, raw)
	if err != nil {
		return 0, "", err
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return 0, "", errors.New("HLS: установите FFmpeg для сборки локального MP4")
	}
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".boosty-hls-")
	if err != nil {
		return 0, "", err
	}
	defer os.RemoveAll(dir)
	var total int64
	videoLocal, err := stageHLSTrack(ctx, fetch, dir, "video", video, &total)
	if err != nil {
		return 0, "", err
	}
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file,pipe,data", "-allowed_extensions", "ALL", "-i", videoLocal}
	if len(audio) > 0 {
		audioLocal, e := stageHLSTrack(ctx, fetch, dir, "audio", audio, &total)
		if e != nil {
			return 0, "", e
		}
		args = append(args, "-protocol_whitelist", "file,pipe,data", "-allowed_extensions", "ALL", "-i", audioLocal, "-map", "0:v:0", "-map", "1:a:0")
	} else {
		args = append(args, "-map", "0:v:0", "-map", "0:a?")
	}
	output := filepath.Join(dir, "remux.mp4")
	args = append(args, "-c", "copy", "-movflags", "+faststart", "-y", output)
	remuxCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	result, err := exec.CommandContext(remuxCtx, ffmpeg, args...).CombinedOutput()
	if err != nil {
		if remuxCtx.Err() != nil {
			return 0, "", remuxCtx.Err()
		}
		msg := strings.TrimSpace(string(result))
		if len(msg) > 350 {
			msg = msg[:350]
		}
		return 0, "", fmt.Errorf("HLS: FFmpeg не смог собрать MP4: %s", msg)
	}
	size, digest, err := fileDigest(output)
	if err != nil {
		return 0, "", err
	}
	if size == 0 || size > maxArchiveMediaSize {
		return 0, "", errors.New("HLS: некорректный размер готового MP4")
	}
	if err := os.Chmod(output, 0600); err != nil {
		return 0, "", err
	}
	if err := os.Rename(output, dest); err != nil {
		return 0, "", err
	}
	return size, digest, nil
}

func downloadVideoFile(ctx context.Context, fetch mediaFetcher, raw, dest string) (int64, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return 0, "", errors.New("неверный URL видео")
	}
	switch strings.ToLower(filepath.Ext(u.Path)) {
	case ".m3u8":
		return archiveHLS(ctx, fetch, raw, dest)
	case ".mpd":
		return archiveDASH(ctx, fetch, raw, dest)
	default:
		// Boosty/CDN URLs may contain an opaque ID instead of .m3u8/.mpd.
		// Probe only unknown extensions, never re-download ordinary MP4 twice.
		if ext := strings.ToLower(filepath.Ext(u.Path)); ext != ".mp4" && ext != ".m4v" && ext != ".mov" && ext != ".webm" && ext != ".mkv" {
			switch detectVideoPlaylist(ctx, fetch, raw) {
			case "hls":
				return archiveHLS(ctx, fetch, raw, dest)
			case "dash":
				return archiveDASH(ctx, fetch, raw, dest)
			}
		}
		return downloadFile(ctx, fetch, raw, dest, 0)
	}
}

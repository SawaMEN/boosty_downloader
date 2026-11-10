package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// hlsMasterAttributeList parses RFC 8216 attribute-lists, where quoted values
// may contain commas. Unknown keys are ignored but malformed quoting fails.
func hlsMasterAttributeList(tag string) (map[string]string, error) {
	out := map[string]string{}
	fields := []string{}
	var field strings.Builder
	inQuote := false
	for _, r := range tag {
		switch r {
		case '"':
			inQuote = !inQuote
			field.WriteRune(r)
		case ',':
			if !inQuote {
				fields = append(fields, field.String())
				field.Reset()
			} else {
				field.WriteRune(r)
			}
		default:
			field.WriteRune(r)
		}
	}
	if inQuote {
		return nil, errors.New("HLS: кавычки в атрибутах master-плейлиста не закрыты")
	}
	fields = append(fields, field.String())
	for _, raw := range fields {
		key, val, ok := strings.Cut(strings.TrimSpace(raw), "=")
		if !ok || key == "" || val == "" {
			return nil, errors.New("HLS: неверные атрибуты master-плейлиста")
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if strings.HasPrefix(val, "\"") {
			if len(val) < 2 || !strings.HasSuffix(val, "\"") {
				return nil, errors.New("HLS: неверные кавычки атрибута")
			}
			val = val[1 : len(val)-1]
		}
		if _, found := out[key]; found {
			return nil, errors.New("HLS: дубликат атрибута")
		}
		out[key] = val
	}
	return out, nil
}

type hlsAudioGroup struct {
	group, uri string
	preferred  bool
}

// resolveHLSWithAudio extends the original finite-HLS implementation with
// standalone AUDIO renditions, keeping segment downloads in the safe transport.
func resolveHLSWithAudio(ctx context.Context, fetch mediaFetcher, raw string) ([]hlsPart, []hlsPart, error) {
	data, err := fetchHLSBytes(ctx, fetch, raw, maxHLSManifestBytes)
	if err != nil {
		return nil, nil, err
	}
	// Ordinary media and master playlists remain on the tested legacy route.
	if !strings.Contains(string(data), "#EXT-X-MEDIA:") {
		v, e := resolveHLS(ctx, fetch, raw)
		return v, nil, e
	}
	scan := bufio.NewScanner(strings.NewReader(strings.TrimPrefix(string(data), "\ufeff")))
	scan.Buffer(make([]byte, 4096), maxHLSManifestBytes+1024)
	header := false
	var pending bool
	var pendingBandwidth int64
	var pendingAudio string
	videoVariants := []struct {
		url, audio string
		bandwidth  int64
	}{}
	audios := []hlsAudioGroup{}
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if !header {
			if line != "#EXTM3U" {
				return nil, nil, errors.New("HLS: неверный master-плейлист")
			}
			header = true
			continue
		}
		if line == "" {
			continue
		}
		if pending && !strings.HasPrefix(line, "#") {
			u, e := hlsResourceURL(raw, line)
			if e != nil {
				return nil, nil, e
			}
			if len(videoVariants) >= 100 {
				return nil, nil, errors.New("HLS: слишком много вариантов")
			}
			videoVariants = append(videoVariants, struct {
				url, audio string
				bandwidth  int64
			}{u, pendingAudio, pendingBandwidth})
			pending = false
			continue
		}
		if pending {
			return nil, nil, errors.New("HLS: отсутствует URL видео после EXT-X-STREAM-INF")
		}
		switch {
		case strings.HasPrefix(line, "#EXT-X-SESSION-KEY:") || strings.HasPrefix(line, "#EXT-X-KEY:"):
			return nil, nil, errors.New("HLS: защищённый поток не поддерживается")
		case strings.HasPrefix(line, "#EXT-X-MEDIA:"):
			attrs, e := hlsMasterAttributeList(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
			if e != nil {
				return nil, nil, e
			}
			if attrs["TYPE"] != "AUDIO" {
				continue
			}
			if attrs["GROUP-ID"] == "" {
				return nil, nil, errors.New("HLS: аудиогруппа без GROUP-ID")
			}
			var loc string
			if attrs["URI"] != "" {
				loc, e = hlsResourceURL(raw, attrs["URI"])
				if e != nil {
					return nil, nil, e
				}
			}
			if len(audios) >= 100 {
				return nil, nil, errors.New("HLS: слишком много аудиодорожек")
			}
			audios = append(audios, hlsAudioGroup{group: attrs["GROUP-ID"], uri: loc, preferred: attrs["DEFAULT"] == "YES"})
		case strings.HasPrefix(line, "#EXT-X-STREAM-INF:"):
			attrs, e := hlsMasterAttributeList(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			if e != nil {
				return nil, nil, e
			}
			pendingBandwidth, _ = strconv.ParseInt(attrs["BANDWIDTH"], 10, 64)
			pendingAudio = attrs["AUDIO"]
			pending = true
		case strings.HasPrefix(line, "#"):
			// Ignore informational master tags, never forward to FFmpeg.
		default:
			return nil, nil, errors.New("HLS: сегмент в master-плейлисте")
		}
	}
	if err := scan.Err(); err != nil {
		return nil, nil, errors.New("HLS: неверный master-плейлист")
	}
	if pending || len(videoVariants) == 0 {
		return nil, nil, errors.New("HLS: отсутствует видеовариант")
	}
	best := videoVariants[0]
	for _, v := range videoVariants[1:] {
		if v.bandwidth > best.bandwidth {
			best = v
		}
	}
	video, e := resolveHLS(ctx, fetch, best.url)
	if e != nil {
		return nil, nil, e
	}
	if best.audio == "" {
		return video, nil, nil
	}
	var selected *hlsAudioGroup
	for _, a := range audios {
		if a.group != best.audio {
			continue
		}
		if selected == nil || a.preferred && !selected.preferred {
			copy := a
			selected = &copy
		}
	}
	if selected == nil {
		return nil, nil, errors.New("HLS: не найдена объявленная аудиогруппа")
	}
	if selected.uri == "" {
		return video, nil, nil
	} // muxed audio per HLS specification
	audio, e := resolveHLS(ctx, fetch, selected.uri)
	if e != nil {
		return nil, nil, e
	}
	return video, audio, nil
}

// stageHLSTrack replaces all remote references with private, generated local
// filenames. Budgets include both audio and video as a single archive.
func stageHLSTrack(ctx context.Context, fetch mediaFetcher, dir, prefix string, parts []hlsPart, total *int64) (string, error) {
	if len(parts) == 0 || len(parts) > maxHLSSegments {
		return "", errors.New("HLS: недопустимое количество сегментов")
	}
	var playlist strings.Builder
	playlist.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-TARGETDURATION:36000\n#EXT-X-MEDIA-SEQUENCE:0\n")
	lastInit := ""
	for i, part := range parts {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if part.discontinuity {
			playlist.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if part.initURL != "" && part.initURL != lastInit {
			name := fmt.Sprintf("%s-init-%06d.mp4", prefix, i)
			n, e := fetchHLSSegment(ctx, fetch, part.initURL, filepath.Join(dir, name), maxArchiveMediaSize-*total)
			if e != nil {
				return "", e
			}
			*total += n
			fmt.Fprintf(&playlist, "#EXT-X-MAP:URI=%q\n", name)
			lastInit = part.initURL
		}
		ext := strings.ToLower(filepath.Ext(strings.Split(part.url, "?")[0]))
		switch ext {
		case ".ts", ".m4s", ".mp4", ".aac":
		default:
			ext = ".ts"
		}
		name := fmt.Sprintf("%s-segment-%06d%s", prefix, i, ext)
		n, e := fetchHLSSegment(ctx, fetch, part.url, filepath.Join(dir, name), maxArchiveMediaSize-*total)
		if e != nil {
			return "", e
		}
		*total += n
		fmt.Fprintf(&playlist, "#EXTINF:%.6f,\n%s\n", part.duration, name)
	}
	playlist.WriteString("#EXT-X-ENDLIST\n")
	local := filepath.Join(dir, prefix+".m3u8")
	if err := os.WriteFile(local, []byte(playlist.String()), 0600); err != nil {
		return "", err
	}
	return local, nil
}

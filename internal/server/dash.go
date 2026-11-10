package server

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Offline DASH support deliberately accepts only finite, unencrypted ISO-BMFF
// presentations. All remote requests use the application's SSRF-protected
// mediaFetcher; the media muxer can open only local, server-created files.
type dashMPD struct {
	XMLName     xml.Name     `xml:"MPD"`
	Type        string       `xml:"type,attr"`
	Duration    string       `xml:"mediaPresentationDuration,attr"`
	BaseURL     string       `xml:"BaseURL"`
	Protections []struct{}   `xml:"ContentProtection"`
	Periods     []dashPeriod `xml:"Period"`
}
type dashPeriod struct {
	Duration    string           `xml:"duration,attr"`
	BaseURL     string           `xml:"BaseURL"`
	Protections []struct{}       `xml:"ContentProtection"`
	Sets        []dashAdaptation `xml:"AdaptationSet"`
}
type dashAdaptation struct {
	ContentType string               `xml:"contentType,attr"`
	MimeType    string               `xml:"mimeType,attr"`
	BaseURL     string               `xml:"BaseURL"`
	Template    *dashTemplate        `xml:"SegmentTemplate"`
	List        *dashSegmentList     `xml:"SegmentList"`
	Protections []struct{}           `xml:"ContentProtection"`
	Reps        []dashRepresentation `xml:"Representation"`
}
type dashRepresentation struct {
	ID          string           `xml:"id,attr"`
	Bandwidth   int64            `xml:"bandwidth,attr"`
	MimeType    string           `xml:"mimeType,attr"`
	BaseURL     string           `xml:"BaseURL"`
	Template    *dashTemplate    `xml:"SegmentTemplate"`
	List        *dashSegmentList `xml:"SegmentList"`
	Protections []struct{}       `xml:"ContentProtection"`
}
type dashTemplate struct {
	Init        string `xml:"initialization,attr"`
	Media       string `xml:"media,attr"`
	StartNumber uint64 `xml:"startNumber,attr"`
	Timescale   uint64 `xml:"timescale,attr"`
	Duration    uint64 `xml:"duration,attr"`
	Timeline    *struct {
		S []dashTimelineEntry `xml:"S"`
	} `xml:"SegmentTimeline"`
}
type dashTimelineEntry struct {
	T *uint64 `xml:"t,attr"`
	D uint64  `xml:"d,attr"`
	R int64   `xml:"r,attr"`
}
type dashSegmentList struct {
	Init struct {
		SourceURL string `xml:"sourceURL,attr"`
		Range     string `xml:"range,attr"`
	} `xml:"Initialization"`
	Entries []struct {
		Media string `xml:"media,attr"`
		Range string `xml:"mediaRange,attr"`
	} `xml:"SegmentURL"`
}
type dashTrack struct {
	init     string
	segments []string
}

// Parsing only H/M/S durations prevents calendar-relative time calculations.
var mpdDurationPattern = regexp.MustCompile(`^PT(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?$`)

func dashDuration(s string) (float64, error) {
	m := mpdDurationPattern.FindStringSubmatch(s)
	if m == nil || s == "PT" {
		return 0, errors.New("DASH: некорректная продолжительность VOD")
	}
	hours, _ := strconv.ParseFloat(m[1], 64)
	mins, _ := strconv.ParseFloat(m[2], 64)
	secs, _ := strconv.ParseFloat(m[3], 64)
	total := hours*3600 + mins*60 + secs
	if math.IsInf(total, 0) || total <= 0 || total > 24*3600*30 {
		return 0, errors.New("DASH: недопустимая продолжительность VOD")
	}
	return total, nil
}

var dashTokenPattern = regexp.MustCompile(`\$\$|\$(?:RepresentationID|Bandwidth|Time|Number(?:%0[1-6]d)?)\$`)

func expandDASHTemplate(s, id string, bandwidth int64, number, tick uint64) (string, error) {
	if s == "" || len(s) > 2048 {
		return "", errors.New("DASH: отсутствует шаблон сегмента")
	}
	out := dashTokenPattern.ReplaceAllStringFunc(s, func(key string) string {
		if key == "$$" {
			return "$"
		}
		token := strings.Trim(key, "$")
		switch {
		case token == "RepresentationID":
			return id
		case token == "Bandwidth":
			return strconv.FormatInt(bandwidth, 10)
		case token == "Time":
			return strconv.FormatUint(tick, 10)
		case token == "Number":
			return strconv.FormatUint(number, 10)
		case strings.HasPrefix(token, "Number%0"):
			w := int(token[8] - '0')
			return fmt.Sprintf("%0*d", w, number)
		}
		return key
	})
	if strings.Contains(out, "$") {
		return "", errors.New("DASH: неподдерживаемый параметр шаблона")
	}
	return out, nil
}

func dashBase(parent, next string) (string, error) {
	if strings.TrimSpace(next) == "" {
		return parent, nil
	}
	return hlsResourceURL(parent, strings.TrimSpace(next))
}
func dashTrackURLs(parent string, rep dashRepresentation, set dashAdaptation, duration float64) (dashTrack, error) {
	var tr dashTrack
	base, err := dashBase(parent, set.BaseURL)
	if err != nil {
		return tr, err
	}
	base, err = dashBase(base, rep.BaseURL)
	if err != nil {
		return tr, err
	}
	if len(rep.Protections) > 0 || len(set.Protections) > 0 {
		return tr, errors.New("DASH: защищённое DRM видео не поддерживается")
	}
	list := rep.List
	if list == nil {
		list = set.List
	}
	if list != nil {
		if rep.Template != nil || set.Template != nil {
			return tr, errors.New("DASH: смешанные методы описания сегментов")
		}
		if list.Init.SourceURL == "" || list.Init.Range != "" || len(list.Entries) == 0 || len(list.Entries) > maxHLSSegments {
			return tr, errors.New("DASH: неподдерживаемый SegmentList")
		}
		tr.init, err = hlsResourceURL(base, list.Init.SourceURL)
		if err != nil {
			return tr, err
		}
		for _, entry := range list.Entries {
			if entry.Media == "" || entry.Range != "" {
				return tr, errors.New("DASH: диапазоны сегментов не поддерживаются")
			}
			u, e := hlsResourceURL(base, entry.Media)
			if e != nil {
				return tr, e
			}
			tr.segments = append(tr.segments, u)
		}
		return tr, nil
	}
	templ := rep.Template
	if templ == nil {
		templ = set.Template
	}
	if templ == nil || templ.Init == "" || templ.Media == "" {
		return tr, errors.New("DASH: требуется SegmentTemplate с initialization и media")
	}
	start := templ.StartNumber
	if start == 0 {
		start = 1
	}
	timescale := templ.Timescale
	if timescale == 0 {
		timescale = 1
	}
	init, e := expandDASHTemplate(templ.Init, rep.ID, rep.Bandwidth, start, 0)
	if e != nil {
		return tr, e
	}
	tr.init, err = hlsResourceURL(base, init)
	if err != nil {
		return tr, err
	}
	type segment struct{ number, tick uint64 }
	segmentIDs := make([]segment, 0)
	if templ.Timeline != nil && len(templ.Timeline.S) > 0 {
		tick := uint64(0)
		for _, item := range templ.Timeline.S {
			if item.T != nil {
				tick = *item.T
			}
			if item.D == 0 || item.R < 0 || item.R > maxHLSSegments {
				return tr, errors.New("DASH: некорректный SegmentTimeline (r=-1 не поддерживается)")
			}
			for k := int64(0); k <= item.R; k++ {
				if len(segmentIDs) >= maxHLSSegments {
					return tr, errors.New("DASH: слишком много сегментов")
				}
				segmentIDs = append(segmentIDs, segment{number: start + uint64(len(segmentIDs)), tick: tick})
				if tick > ^uint64(0)-item.D {
					return tr, errors.New("DASH: переполнение времени сегмента")
				}
				tick += item.D
			}
		}
	} else {
		if templ.Duration == 0 {
			return tr, errors.New("DASH: неизвестна длительность сегментов")
		}
		countF := math.Ceil(duration*float64(timescale)/float64(templ.Duration) - 1e-8)
		if countF < 1 || countF > maxHLSSegments {
			return tr, errors.New("DASH: слишком много сегментов")
		}
		for k := 0; k < int(countF); k++ {
			if uint64(k) > ^uint64(0)/templ.Duration {
				return tr, errors.New("DASH: переполнение сегмента")
			}
			segmentIDs = append(segmentIDs, segment{number: start + uint64(k), tick: uint64(k) * templ.Duration})
		}
	}
	for _, part := range segmentIDs {
		name, e := expandDASHTemplate(templ.Media, rep.ID, rep.Bandwidth, part.number, part.tick)
		if e != nil {
			return tr, e
		}
		u, e := hlsResourceURL(base, name)
		if e != nil {
			return tr, e
		}
		tr.segments = append(tr.segments, u)
	}
	return tr, nil
}

// resolveDASH returns the highest-bandwidth supported video and audio tracks.
// The source manifest is not passed to external programs or written into archives.
func resolveDASH(ctx context.Context, fetch mediaFetcher, raw string) (dashTrack, *dashTrack, error) {
	var empty dashTrack
	b, err := fetchHLSBytes(ctx, fetch, raw, maxHLSManifestBytes)
	if err != nil {
		return empty, nil, err
	}
	if strings.Contains(strings.ToLower(string(b)), "<!doctype") || strings.Contains(strings.ToLower(string(b)), "<!entity") {
		return empty, nil, errors.New("DASH: XML entities запрещены")
	}
	var doc dashMPD
	if err := xml.Unmarshal(b, &doc); err != nil {
		return empty, nil, fmt.Errorf("DASH: неправильный MPD: %w", err)
	}
	if doc.XMLName.Local != "MPD" || doc.Type != "static" || len(doc.Periods) != 1 || len(doc.Protections) > 0 {
		return empty, nil, errors.New("DASH: поддерживается один период без DRM в конечном MPD")
	}
	p := doc.Periods[0]
	if len(p.Protections) > 0 {
		return empty, nil, errors.New("DASH: DRM не поддерживается")
	}
	durationText := p.Duration
	if durationText == "" {
		durationText = doc.Duration
	}
	duration, err := dashDuration(durationText)
	if err != nil {
		return empty, nil, err
	}
	base, err := dashBase(raw, doc.BaseURL)
	if err != nil {
		return empty, nil, err
	}
	base, err = dashBase(base, p.BaseURL)
	if err != nil {
		return empty, nil, err
	}
	var video, audio *dashTrack
	var videoRate, audioRate int64 = -1, -1
	for _, set := range p.Sets {
		kind := set.ContentType
		if kind == "" {
			if strings.HasPrefix(set.MimeType, "video/") {
				kind = "video"
			} else if strings.HasPrefix(set.MimeType, "audio/") {
				kind = "audio"
			}
		}
		for _, rep := range set.Reps {
			mt := rep.MimeType
			if mt == "" {
				mt = set.MimeType
			}
			if kind == "" {
				if strings.HasPrefix(mt, "video/") {
					kind = "video"
				} else if strings.HasPrefix(mt, "audio/") {
					kind = "audio"
				}
			}
			if kind != "video" && kind != "audio" {
				continue
			}
			if mt != "video/mp4" && mt != "audio/mp4" {
				continue
			} // webm is not ISO BMFF
			if kind == "video" && rep.Bandwidth <= videoRate || kind == "audio" && rep.Bandwidth <= audioRate {
				continue
			}
			track, e := dashTrackURLs(base, rep, set, duration)
			if e != nil {
				return empty, nil, e
			} // do not quietly skip protected high-quality sources
			if kind == "video" {
				video = &track
				videoRate = rep.Bandwidth
			} else {
				audio = &track
				audioRate = rep.Bandwidth
			}
		}
	}
	if video == nil {
		return empty, nil, errors.New("DASH: нет поддерживаемой видеодорожки ISO BMFF")
	}
	if len(video.segments)+func() int {
		if audio != nil {
			return len(audio.segments)
		}
		return 0
	}() > maxHLSSegments {
		return empty, nil, errors.New("DASH: слишком много сегментов")
	}
	return *video, audio, nil
}

func fetchDASHTrack(ctx context.Context, fetch mediaFetcher, tr dashTrack, dir, name string, budget *int64) (string, error) {
	path := filepath.Join(dir, name)
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	// Each segment is downloaded into a distinct staging file, then appended
	// to the local ISO BMFF track. The FFmpeg process never receives a URL.
	defer output.Close()
	links := append([]string{tr.init}, tr.segments...)
	for i, link := range links {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		temp := filepath.Join(dir, fmt.Sprintf(".dash-%s-%06d", name, i))
		n, e := fetchHLSSegment(ctx, fetch, link, temp, *budget)
		if e != nil {
			return "", e
		}
		*budget -= n
		part, e := os.Open(temp)
		if e != nil {
			return "", e
		}
		_, cpErr := guardedCopy(ctx, output, part)
		closeErr := part.Close()
		removeErr := os.Remove(temp)
		if removeErr == nil {
			releaseStorageBudget(ctx, n)
		}
		if cpErr != nil {
			return "", cpErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	if err := output.Sync(); err != nil {
		return "", err
	}
	return path, nil
}

// archiveDASH keeps existing destinations intact until both tracks are fully
// fetched, muxed and SHA-256 verified. Unsupported live or DRM returns an error.
func archiveDASH(ctx context.Context, fetch mediaFetcher, raw, dest string) (int64, string, error) {
	video, audio, err := resolveDASH(ctx, fetch, raw)
	if err != nil {
		return 0, "", err
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return 0, "", errors.New("DASH: необходим FFmpeg")
	}
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".boosty-dash-")
	if err != nil {
		return 0, "", err
	}
	defer os.RemoveAll(dir)
	budget := maxArchiveMediaSize
	videoFile, err := fetchDASHTrack(ctx, fetch, video, dir, "video.mp4", &budget)
	if err != nil {
		return 0, "", err
	}
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file,pipe,data", "-i", videoFile}
	if audio != nil {
		audioFile, e := fetchDASHTrack(ctx, fetch, *audio, dir, "audio.mp4", &budget)
		if e != nil {
			return 0, "", e
		}
		args = append(args, "-i", audioFile, "-map", "0:v:0", "-map", "1:a:0")
	} else {
		args = append(args, "-map", "0:v:0", "-map", "0:a?")
	}
	output := filepath.Join(dir, "remux.mp4")
	args = append(args, "-c", "copy", "-movflags", "+faststart", "-y", output)
	muxCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := checkStorageBudget(ctx, maxArchiveMediaSize-budget); err != nil {
		return 0, "", err
	}
	stdout, e := exec.CommandContext(muxCtx, ffmpeg, args...).CombinedOutput()
	if e != nil {
		if muxCtx.Err() != nil {
			return 0, "", muxCtx.Err()
		}
		msg := strings.TrimSpace(string(stdout))
		if len(msg) > 350 {
			msg = msg[:350]
		}
		return 0, "", fmt.Errorf("DASH: FFmpeg не смог собрать MP4: %s", msg)
	}
	size, digest, err := fileDigest(output)
	if err != nil {
		return 0, "", err
	}
	if size == 0 || size > maxArchiveMediaSize {
		return 0, "", errors.New("DASH: некорректный размер MP4")
	}
	if err := os.Chmod(output, 0600); err != nil {
		return 0, "", err
	}
	if err := os.Rename(output, dest); err != nil {
		return 0, "", err
	}
	return size, digest, nil
}

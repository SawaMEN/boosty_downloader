package server

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
	Start       string           `xml:"start,attr"`
	BaseURL     string           `xml:"BaseURL"`
	Template    *dashTemplate    `xml:"SegmentTemplate"`
	List        *dashSegmentList `xml:"SegmentList"`
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
	Init                   *string `xml:"initialization,attr"`
	Media                  *string `xml:"media,attr"`
	StartNumber            *uint64 `xml:"startNumber,attr"`
	Timescale              *uint64 `xml:"timescale,attr"`
	Duration               *uint64 `xml:"duration,attr"`
	PresentationTimeOffset *uint64 `xml:"presentationTimeOffset,attr"`
	Timeline               *struct {
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
	var out strings.Builder
	pos := 0
	for _, match := range dashTokenPattern.FindAllStringIndex(s, -1) {
		literal := s[pos:match[0]]
		if strings.Contains(literal, "$") {
			return "", errors.New("DASH: неподдерживаемый параметр шаблона")
		}
		out.WriteString(literal)
		key := s[match[0]:match[1]]
		pos = match[1]
		if key == "$$" {
			out.WriteByte('$')
			continue
		}
		token := strings.Trim(key, "$")
		switch {
		case token == "RepresentationID":
			if strings.Contains(id, "$") {
				return "", errors.New("DASH: неверный идентификатор Representation")
			}
			out.WriteString(id)
		case token == "Bandwidth":
			out.WriteString(strconv.FormatInt(bandwidth, 10))
		case token == "Time":
			out.WriteString(strconv.FormatUint(tick, 10))
		case token == "Number":
			out.WriteString(strconv.FormatUint(number, 10))
		case strings.HasPrefix(token, "Number%0"):
			w := int(token[8] - '0')
			fmt.Fprintf(&out, "%0*d", w, number)
		}
	}
	if strings.Contains(s[pos:], "$") {
		return "", errors.New("DASH: неподдерживаемый параметр шаблона")
	}
	out.WriteString(s[pos:])
	return out.String(), nil
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
	templ := mergeDASHTemplate(set.Template, rep.Template)
	if templ == nil || templ.Init == nil || templ.Media == nil || *templ.Init == "" || *templ.Media == "" {
		return tr, errors.New("DASH: требуется SegmentTemplate с initialization и media")
	}
	start := uint64(1)
	if templ.StartNumber != nil {
		start = *templ.StartNumber
	}
	timescale := uint64(1)
	if templ.Timescale != nil {
		timescale = *templ.Timescale
	}
	if timescale == 0 {
		return tr, errors.New("DASH: нулевой timescale")
	}
	if templ.PresentationTimeOffset != nil && *templ.PresentationTimeOffset != 0 {
		return tr, errors.New("DASH: ненулевой presentationTimeOffset пока не поддерживается")
	}
	init, e := expandDASHTemplate(*templ.Init, rep.ID, rep.Bandwidth, start, 0)
	if e != nil {
		return tr, e
	}
	tr.init, err = hlsResourceURL(base, init)
	if err != nil {
		return tr, err
	}
	type segment struct{ number, tick uint64 }
	segmentIDs := make([]segment, 0)
	if templ.Timeline != nil {
		if len(templ.Timeline.S) == 0 {
			return tr, errors.New("DASH: пустой SegmentTimeline")
		}
		tick := uint64(0)
		for index, item := range templ.Timeline.S {
			if item.T != nil {
				if len(segmentIDs) != 0 && *item.T < tick {
					return tr, errors.New("DASH: пересекающиеся или обратные интервалы SegmentTimeline")
				}
				tick = *item.T
			}
			if item.D == 0 || item.R < -1 || item.R > maxHLSSegments {
				return tr, errors.New("DASH: некорректный SegmentTimeline")
			}
			repetitions := uint64(item.R) + 1
			if item.R == -1 {
				// MPEG-DASH r=-1 repeats until the next explicit S@t,
				// or the end of a finite Period for the final S. Never
				// extrapolate an unbounded live timeline.
				var end uint64
				if index+1 < len(templ.Timeline.S) {
					next := templ.Timeline.S[index+1].T
					if next == nil {
						return tr, errors.New("DASH: r=-1 требует следующий S@t")
					}
					end = *next
				} else {
					endFloat := math.Ceil(duration * float64(timescale))
					if math.IsNaN(endFloat) || math.IsInf(endFloat, 0) || endFloat >= math.Exp2(64) {
						return tr, errors.New("DASH: переполнение конца периода")
					}
					end = uint64(endFloat)
				}
				if end <= tick {
					return tr, errors.New("DASH: r=-1 начинается за концом интервала")
				}
				// ceil((end-tick)/d) using integers, without overflowing
				// end-tick+d-1 or allocating an unbounded number of URLs.
				repetitions = (end-tick-1)/item.D + 1
			}
			if repetitions > uint64(maxHLSSegments-len(segmentIDs)) {
				return tr, errors.New("DASH: слишком много сегментов")
			}
			if start > ^uint64(0)-(uint64(len(segmentIDs))+repetitions-1) {
				return tr, errors.New("DASH: переполнение номера сегмента")
			}
			for k := uint64(0); k < repetitions; k++ {
				segmentIDs = append(segmentIDs, segment{number: start + uint64(len(segmentIDs)), tick: tick})
				if tick > ^uint64(0)-item.D {
					return tr, errors.New("DASH: переполнение времени сегмента")
				}
				tick += item.D
			}
		}
	} else {
		if templ.Duration == nil || *templ.Duration == 0 {
			return tr, errors.New("DASH: неизвестна длительность сегментов")
		}
		countF := math.Ceil(duration*float64(timescale)/float64(*templ.Duration) - 1e-8)
		if countF < 1 || countF > maxHLSSegments {
			return tr, errors.New("DASH: слишком много сегментов")
		}
		if start > ^uint64(0)-uint64(int(countF)-1) {
			return tr, errors.New("DASH: переполнение номера сегмента")
		}
		for k := 0; k < int(countF); k++ {
			if uint64(k) > ^uint64(0)/(*templ.Duration) {
				return tr, errors.New("DASH: переполнение сегмента")
			}
			segmentIDs = append(segmentIDs, segment{number: start + uint64(k), tick: uint64(k) * (*templ.Duration)})
		}
	}
	for _, part := range segmentIDs {
		name, e := expandDASHTemplate(*templ.Media, rep.ID, rep.Bandwidth, part.number, part.tick)
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

// resolveDASH retains the single-period resolver contract for existing callers.
func resolveDASH(ctx context.Context, fetch mediaFetcher, raw string) (dashTrack, *dashTrack, error) {
	periods, err := resolveDASHPresentation(ctx, fetch, raw)
	if err != nil {
		return dashTrack{}, nil, err
	}
	if len(periods) != 1 {
		return dashTrack{}, nil, errors.New("DASH: требуется многопериодная сборка")
	}
	return periods[0].video, periods[0].audio, nil
}

// Resolve each period independently, including its own initialization files.
func resolveDASHPeriod(base string, p dashPeriod, duration float64) (dashTrack, *dashTrack, error) {
	var empty dashTrack
	if len(p.Protections) > 0 {
		return empty, nil, errors.New("DASH: DRM не поддерживается")
	}
	base, err := dashBase(base, p.BaseURL)
	if err != nil {
		return empty, nil, err
	}
	var video, audio *dashTrack
	var videoRate, audioRate int64 = -1, -1
	for _, set := range p.Sets {
		if p.List != nil {
			return empty, nil, errors.New("DASH: SegmentList на уровне Period пока не поддерживается")
		}
		set.Template = mergeDASHTemplate(p.Template, set.Template)
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
	for i := 0; i < len(links); {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		count := 1
		if segmentWorkers(ctx, "BD_DASH_SEGMENT_WORKERS") == 2 && len(links)-i >= 2 && *budget >= 2*maxHLSSegmentBytes {
			count = 2
		}
		var batch [2]mediaSegmentTask
		for n := 0; n < count; n++ {
			batch[n] = mediaSegmentTask{url: links[i+n], path: filepath.Join(dir, fmt.Sprintf(".dash-%s-%06d", name, i+n))}
		}
		sizes, err := fetchMediaSegmentBatch(ctx, fetch, batch[:count], *budget)
		if err != nil {
			return "", err
		}
		for n, task := range batch[:count] {
			*budget -= sizes[n]
			if err := appendDASHSegment(ctx, output, task.path, sizes[n]); err != nil {
				return "", err
			}
		}
		i += count
	}
	if err := output.Sync(); err != nil {
		return "", err
	}
	return path, nil
}

// archiveDASH keeps existing destinations intact until both tracks are fully
// fetched, muxed and SHA-256 verified. Unsupported live or DRM returns an error.
func archiveDASH(ctx context.Context, fetch mediaFetcher, raw, dest string) (int64, string, error) {
	periods, err := resolveDASHPresentation(ctx, fetch, raw)
	if err != nil {
		return 0, "", err
	}
	if len(periods) > 1 {
		return archiveDASHPeriods(ctx, fetch, periods, dest)
	}
	video, audio := periods[0].video, periods[0].audio
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return 0, "", errors.New("DASH: необходим FFmpeg")
	}
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".boosty-dash-")
	if err != nil {
		return 0, "", err
	}
	ctx = withMediaStageBudget(ctx)
	defer cleanupMediaStage(ctx, dir)
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
		args = append(args, "-protocol_whitelist", "file,pipe,data", "-i", audioFile, "-map", "0:v:0", "-map", "1:a:0")
	} else {
		args = append(args, "-map", "0:v:0", "-map", "0:a?")
	}
	output := filepath.Join(dir, "remux.mp4")
	args = append(args, "-c", "copy", "-movflags", "+faststart", "-y", output)
	if err := runBoundedMediaMux(ctx, ffmpeg, args, output, maxArchiveMediaSize); err != nil {
		return 0, "", err
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
	if err := publishStagedVideo(ctx, output, dest, size); err != nil {
		return 0, "", err
	}
	return size, digest, nil
}

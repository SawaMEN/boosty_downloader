package server

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const maxDASHPeriods = 32
const dashBoundaryTolerance = 0.000001

type dashPeriodPlan struct {
	video    dashTrack
	audio    *dashTrack
	duration float64
}

func dashPeriodStart(text string) (float64, error) {
	if mpdDurationPattern.MatchString(text) && text != "PT" && !strings.ContainsAny(text, "123456789") {
		return 0, nil
	}
	return dashDuration(text)
}

// Multi-period support is deliberately restricted to a contiguous finite
// presentation. Missing durations can be inferred from the next explicit
// start or MPD duration; gaps, overlaps and ambiguous boundaries fail closed.
func dashPeriodDurations(doc dashMPD) ([]float64, error) {
	if len(doc.Periods) == 0 || len(doc.Periods) > maxDASHPeriods {
		return nil, errors.New("DASH: недопустимое число периодов")
	}
	var total float64
	var err error
	if doc.Duration != "" {
		total, err = dashDuration(doc.Duration)
		if err != nil {
			return nil, err
		}
	}
	durations := make([]float64, len(doc.Periods))
	cursor := 0.0
	for i, period := range doc.Periods {
		if period.Start != "" {
			start, err := dashPeriodStart(period.Start)
			if err != nil || math.Abs(start-cursor) > dashBoundaryTolerance {
				return nil, errors.New("DASH: разрыв или пересечение периодов")
			}
		}
		var duration float64
		switch {
		case period.Duration != "":
			duration, err = dashDuration(period.Duration)
		case i+1 < len(doc.Periods):
			var end float64
			end, err = dashPeriodStart(doc.Periods[i+1].Start)
			duration = end - cursor
		case total > 0:
			duration = total - cursor
		default:
			err = errors.New("DASH: неизвестная граница периода")
		}
		if err != nil || duration <= dashBoundaryTolerance || cursor+duration > 24*3600*30 {
			return nil, errors.New("DASH: недопустимая длительность периода")
		}
		durations[i] = duration
		cursor += duration
	}
	if total > 0 && math.Abs(cursor-total) > dashBoundaryTolerance {
		return nil, errors.New("DASH: длительность периодов не совпадает с MPD")
	}
	return durations, nil
}

func resolveDASHPresentation(ctx context.Context, fetch mediaFetcher, raw string) ([]dashPeriodPlan, error) {
	b, err := fetchHLSBytes(ctx, fetch, raw, maxHLSManifestBytes)
	if err != nil {
		return nil, err
	}
	lower := strings.ToLower(string(b))
	if strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<!entity") {
		return nil, errors.New("DASH: XML entities запрещены")
	}
	var doc dashMPD
	if err := xml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("DASH: неправильный MPD: %w", err)
	}
	if doc.XMLName.Local != "MPD" || doc.Type != "static" || len(doc.Protections) > 0 {
		return nil, errors.New("DASH: требуется конечный MPD без DRM")
	}
	durations, err := dashPeriodDurations(doc)
	if err != nil {
		return nil, err
	}
	base, err := dashBase(raw, doc.BaseURL)
	if err != nil {
		return nil, err
	}
	plans := make([]dashPeriodPlan, 0, len(doc.Periods))
	segments := 0
	for i, p := range doc.Periods {
		video, audio, err := resolveDASHPeriod(base, p, durations[i])
		if err != nil {
			return nil, err
		}
		if len(plans) != 0 && (plans[0].audio == nil) != (audio == nil) {
			return nil, errors.New("DASH: состав дорожек меняется между периодами")
		}
		segments += len(video.segments)
		if audio != nil {
			segments += len(audio.segments)
		}
		if segments > maxHLSSegments {
			return nil, errors.New("DASH: общий лимит сегментов превышен")
		}
		plans = append(plans, dashPeriodPlan{video: video, audio: audio, duration: durations[i]})
	}
	return plans, nil
}

// Only compare stable codec/configuration fields, not per-period durations
// or bitrates. A missing codec configuration hash is rejected conservatively.
type dashProbeStream struct {
	CodecType      string `json:"codec_type"`
	CodecName      string `json:"codec_name"`
	Profile        string `json:"profile"`
	Level          int    `json:"level"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	PixelFormat    string `json:"pix_fmt"`
	Aspect         string `json:"sample_aspect_ratio"`
	FieldOrder     string `json:"field_order"`
	ColorRange     string `json:"color_range"`
	ColorSpace     string `json:"color_space"`
	ColorTransfer  string `json:"color_transfer"`
	ColorPrimaries string `json:"color_primaries"`
	ChromaLocation string `json:"chroma_location"`
	SampleFormat   string `json:"sample_fmt"`
	SampleRate     string `json:"sample_rate"`
	Channels       int    `json:"channels"`
	ChannelLayout  string `json:"channel_layout"`
	TimeBase       string `json:"time_base"`
	ExtraDataHash  string `json:"extradata_hash"`
}

func probeDASHPeriod(ctx context.Context, ffprobe, path string, duration float64) ([]dashProbeStream, error) {
	fields := "stream=codec_type,codec_name,profile,level,width,height,pix_fmt,sample_aspect_ratio,field_order,color_range,color_space,color_transfer,color_primaries,chroma_location,sample_fmt,sample_rate,channels,channel_layout,time_base,extradata_hash:format=duration"
	raw, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-protocol_whitelist", "file,pipe,data", "-show_streams", "-show_data_hash", "sha256", "-show_entries", fields, "-of", "json", path).Output()
	if err != nil {
		return nil, errors.New("DASH: не удалось проверить локальные дорожки")
	}
	var result struct {
		Streams []dashProbeStream `json:"streams"`
		Format  struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	actual, err := strconv.ParseFloat(result.Format.Duration, 64)
	if err != nil || math.IsNaN(actual) || math.IsInf(actual, 0) || actual <= 0 || math.Abs(actual-duration) > 0.5 {
		return nil, errors.New("DASH: длительность локального файла не совпадает с периодом")
	}
	if len(result.Streams) == 0 || len(result.Streams) > 2 || result.Streams[0].CodecType != "video" ||
		(len(result.Streams) == 2 && result.Streams[1].CodecType != "audio") {
		return nil, errors.New("DASH: неподдерживаемый состав локальных дорожек")
	}
	for _, stream := range result.Streams {
		if stream.CodecName == "" || stream.TimeBase == "" || !strings.HasPrefix(stream.ExtraDataHash, "SHA256:") {
			return nil, errors.New("DASH: невозможно подтвердить совместимость кодека")
		}
	}
	return result.Streams, nil
}

// All filenames are generated locally, with a shared download budget. Each
// period is remuxed independently; concat never sees remote playlist text.
func archiveDASHPeriods(ctx context.Context, fetch mediaFetcher, plans []dashPeriodPlan, dest string) (int64, string, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return 0, "", errors.New("DASH: необходим FFmpeg")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0, "", errors.New("DASH: для нескольких периодов необходим FFprobe")
	}
	ctx, cancelAll := context.WithTimeout(ctx, 30*time.Minute)
	defer cancelAll()
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".boosty-dash-periods-")
	if err != nil {
		return 0, "", err
	}
	ctx = withMediaStageBudget(ctx)
	defer cleanupMediaStage(ctx, dir)
	budget := maxArchiveMediaSize
	var signature []dashProbeStream
	var list strings.Builder
	var stagedBytes int64
	var duration float64
	for i, plan := range plans {
		prefix := fmt.Sprintf("period-%03d", i)
		video, err := fetchDASHTrack(ctx, fetch, plan.video, dir, prefix+"-video.mp4", &budget)
		if err != nil {
			return 0, "", err
		}
		args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file,pipe,data", "-i", video}
		if plan.audio != nil {
			audio, err := fetchDASHTrack(ctx, fetch, *plan.audio, dir, prefix+"-audio.mp4", &budget)
			if err != nil {
				return 0, "", err
			}
			args = append(args, "-protocol_whitelist", "file,pipe,data", "-i", audio, "-map", "0:v:0", "-map", "1:a:0")
		} else {
			args = append(args, "-map", "0:v:0", "-map", "0:a:0?")
		}
		name := prefix + ".mp4"
		output := filepath.Join(dir, name)
		args = append(args, "-c", "copy", "-movflags", "+faststart", "-y", output)
		if err := runBoundedMediaMux(ctx, ffmpeg, args, output, maxArchiveMediaSize-stagedBytes); err != nil {
			return 0, "", err
		}
		info, err := os.Stat(output)
		if err != nil || info.Size() <= 0 || info.Size() > maxArchiveMediaSize-stagedBytes {
			return 0, "", errors.New("DASH: недопустимый размер периода")
		}
		if err := chargeIntermediateMediaOutput(ctx, info.Size()); err != nil {
			return 0, "", err
		}
		stagedBytes += info.Size()
		probeCtx, cancel := context.WithTimeout(ctx, time.Minute)
		streams, err := probeDASHPeriod(probeCtx, ffprobe, output, plan.duration)
		cancel()
		if err != nil {
			return 0, "", err
		}
		if i == 0 {
			signature = streams
		} else if !reflect.DeepEqual(signature, streams) {
			return 0, "", errors.New("DASH: несовместимые кодеки или параметры периодов")
		}
		fmt.Fprintf(&list, "file '%s'\n", name)
		duration += plan.duration
	}
	listPath := filepath.Join(dir, "concat.txt")
	if err := os.WriteFile(listPath, []byte(list.String()), 0600); err != nil {
		return 0, "", err
	}
	output := filepath.Join(dir, "remux.mp4")
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-protocol_whitelist", "file,pipe,data", "-f", "concat", "-safe", "1", "-i", listPath, "-map", "0:v:0", "-map", "0:a:0?", "-c", "copy", "-movflags", "+faststart", "-y", output}
	if err := runBoundedMediaMux(ctx, ffmpeg, args, output, maxArchiveMediaSize); err != nil {
		return 0, "", err
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Minute)
	streams, err := probeDASHPeriod(probeCtx, ffprobe, output, duration)
	cancel()
	if err != nil || !reflect.DeepEqual(signature, streams) {
		return 0, "", errors.New("DASH: итоговое видео не прошло проверку дорожек")
	}
	size, digest, err := fileDigest(output)
	if err != nil {
		return 0, "", err
	}
	if size <= 0 || size > maxArchiveMediaSize {
		return 0, "", errors.New("DASH: недопустимый размер MP4")
	}
	if err := os.Chmod(output, 0600); err != nil {
		return 0, "", err
	}
	if err := publishStagedVideo(ctx, output, dest, size); err != nil {
		return 0, "", err
	}
	return size, digest, nil
}

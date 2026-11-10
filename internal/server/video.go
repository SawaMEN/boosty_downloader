package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SawaMEN/boosty_downloader/internal/boosty"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Video conversion never overwrites the original. Hardware acceleration is
// opt-in and tested with an actual one-frame encode, not inferred from a PCI ID.
type VideoPolicy struct {
	Enabled     bool   `json:"enabled"`
	Automatic   bool   `json:"automatic"`
	Backend     string `json:"backend"`
	Codec       string `json:"codec"`
	Quality     int    `json:"quality"`
	MaxHeight   int    `json:"maxHeight"`
	FallbackCPU bool   `json:"fallbackCpu"`
	OnlySmaller bool   `json:"onlySmaller"`
}

func defaultVideoPolicy() VideoPolicy {
	return VideoPolicy{Backend: "auto", Codec: "h264", Quality: 26, MaxHeight: 1080, FallbackCPU: true, OnlySmaller: true}
}
func validateVideoPolicy(p VideoPolicy) error {
	switch p.Backend {
	case "auto", "cpu", "vaapi", "qsv", "nvenc":
	default:
		return errors.New("недопустимый видеоускоритель")
	}
	switch p.Codec {
	case "h264", "hevc":
	default:
		return errors.New("допустимы кодеки H.264 или HEVC")
	}
	if p.Quality < 16 || p.Quality > 40 {
		return errors.New("качество видео должно быть от 16 до 40")
	}
	if p.MaxHeight != 0 && (p.MaxHeight < 240 || p.MaxHeight > 4320) {
		return errors.New("максимальная высота: 0 либо 240–4320")
	}
	if p.Automatic && !p.Enabled {
		return errors.New("для фонового перекодирования включите обработку видео")
	}
	return nil
}

type VideoBackend struct {
	Name      string `json:"name"`
	Encoder   string `json:"encoder"`
	Device    string `json:"device,omitempty"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}
type VideoDiagnostics struct {
	FFmpeg        string         `json:"ffmpeg,omitempty"`
	FFprobe       bool           `json:"ffprobe"`
	RenderDevices []string       `json:"renderDevices"`
	Backends      []VideoBackend `json:"backends"`
	CheckedAt     time.Time      `json:"checkedAt"`
}
type VideoTask struct {
	JobID, Filename string
	Force           bool
}
type VideoJobStatus struct {
	State     string    `json:"state"`
	Filename  string    `json:"filename"`
	Backend   string    `json:"backend,omitempty"`
	Message   string    `json:"message,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

var renderNodeRE = regexp.MustCompile(`^renderD[0-9]+$`)

func renderDevices() []string {
	entries, err := os.ReadDir("/dev/dri")
	if err != nil {
		return []string{}
	}
	out := make([]string, 0)
	for _, e := range entries {
		if renderNodeRE.MatchString(e.Name()) {
			out = append(out, filepath.Join("/dev/dri", e.Name()))
		}
	}
	sort.Strings(out)
	return out
}
func videoEncoder(backend, codec string) string {
	kind := "h264"
	if codec == "hevc" {
		kind = "hevc"
	}
	switch backend {
	case "vaapi":
		return kind + "_vaapi"
	case "qsv":
		return kind + "_qsv"
	case "nvenc":
		return kind + "_nvenc"
	case "cpu":
		if kind == "h264" {
			return "libx264"
		}
		return "libx265"
	}
	return ""
}
func chooseRenderDevice(devices []string, backend string) string {
	if len(devices) == 0 {
		return ""
	}
	vendor := "0x1002"
	if backend == "qsv" {
		vendor = "0x8086"
	}
	for _, device := range devices {
		b, err := os.ReadFile(filepath.Join("/sys/class/drm", filepath.Base(device), "device/vendor"))
		if err == nil && strings.EqualFold(strings.TrimSpace(string(b)), vendor) {
			return device
		}
	}
	return devices[0] // Actual encode probe must still succeed.
}
func probeOne(ctx context.Context, ffmpeg, backend, codec, device string) error {
	encoder := videoEncoder(backend, codec)
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=black:s=128x128:r=1", "-frames:v", "1"}
	if backend == "vaapi" {
		args = append([]string{"-vaapi_device", device}, args...)
		args = append(args, "-vf", "format=nv12,hwupload")
	}
	if backend == "qsv" {
		args = append(args, "-vf", "format=nv12")
	}
	args = append(args, "-c:v", encoder, "-f", "null", "-")
	test, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(test, ffmpeg, args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if len(msg) > 220 {
		msg = msg[:220]
	}
	if test.Err() != nil {
		return errors.New("кодирование превысило время проверки")
	}
	if msg == "" {
		msg = err.Error()
	}
	return errors.New(msg)
}
func probeVideo(ctx context.Context, codec string, active bool) VideoDiagnostics {
	result := VideoDiagnostics{CheckedAt: time.Now().UTC(), RenderDevices: renderDevices(), Backends: []VideoBackend{}}
	if _, err := exec.LookPath("ffprobe"); err == nil {
		result.FFprobe = true
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		result.Backends = []VideoBackend{{Name: "cpu", Reason: "Установите ffmpeg"}}
		return result
	}
	result.FFmpeg = ffmpeg
	infoCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	listing, err := exec.CommandContext(infoCtx, ffmpeg, "-hide_banner", "-encoders").Output()
	if err != nil {
		result.Backends = []VideoBackend{{Name: "cpu", Reason: "FFmpeg не смог перечислить кодеки"}}
		return result
	}
	for _, backend := range []string{"cpu", "vaapi", "qsv", "nvenc"} {
		encoder := videoEncoder(backend, codec)
		item := VideoBackend{Name: backend, Encoder: encoder}
		switch backend {
		case "vaapi", "qsv":
			item.Device = chooseRenderDevice(result.RenderDevices, backend)
			if item.Device == "" {
				item.Reason = "Нет /dev/dri/renderD*: проверьте драйвер, группу render и Docker devices"
			}
		case "nvenc":
			if _, err := os.Stat("/dev/nvidia0"); err != nil {
				item.Reason = "Нет /dev/nvidia0: проверьте драйвер NVIDIA и Container Toolkit"
			}
		}
		if !bytes.Contains(listing, []byte(" "+encoder+" ")) && !bytes.Contains(listing, []byte(" "+encoder+"\n")) {
			item.Reason = "Этот FFmpeg собран без " + encoder
		}
		if item.Reason == "" {
			if active {
				if err := probeOne(ctx, ffmpeg, backend, codec, item.Device); err != nil {
					item.Reason = err.Error()
				} else {
					item.Available = true
				}
			}
			if !active {
				item.Reason = "Требуется пробное кодирование"
			}
		}
		result.Backends = append(result.Backends, item)
	}
	return result
}
func preferredBackends(p VideoPolicy, d VideoDiagnostics) []VideoBackend {
	byName := map[string]VideoBackend{}
	for _, v := range d.Backends {
		byName[v.Name] = v
	}
	if p.Backend != "auto" {
		out := []VideoBackend{byName[p.Backend]}
		if p.FallbackCPU && p.Backend != "cpu" {
			out = append(out, byName["cpu"])
		}
		return out
	}
	order := []string{"nvenc", "qsv", "vaapi", "cpu"}
	// Try QSV first for Intel and VA-API first for AMD.
	if len(d.RenderDevices) > 0 {
		device := chooseRenderDevice(d.RenderDevices, "vaapi")
		vendor, e := os.ReadFile(filepath.Join("/sys/class/drm", filepath.Base(device), "device/vendor"))
		if e == nil && strings.TrimSpace(string(vendor)) == "0x1002" {
			order = []string{"vaapi", "nvenc", "qsv", "cpu"}
		}
	}
	result := []VideoBackend{}
	for _, name := range order {
		if name == "cpu" && !p.FallbackCPU {
			continue
		}
		if v, ok := byName[name]; ok {
			result = append(result, v)
		}
	}
	return result
}

// buildVideoArgs only interpolates validated policy values and server-owned
// absolute paths. No shell or user-controlled FFmpeg option strings are used.
func buildVideoArgs(p VideoPolicy, backend, device, src, dest string, width, height int) []string {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}
	if backend == "vaapi" {
		args = append(args, "-vaapi_device", device)
	}
	args = append(args, "-i", src, "-map", "0:v:0", "-map", "0:a?", "-map_metadata", "-1", "-map_chapters", "-1")
	filters := []string{}
	if p.MaxHeight > 0 && height > p.MaxHeight && width > 0 {
		targetWidth := int(float64(width) * float64(p.MaxHeight) / float64(height))
		targetWidth -= targetWidth % 2
		if targetWidth < 2 {
			targetWidth = 2
		}
		filters = append(filters, fmt.Sprintf("scale=%d:%d", targetWidth, p.MaxHeight-p.MaxHeight%2))
	}
	switch backend {
	case "vaapi":
		filters = append(filters, "format=nv12", "hwupload")
	case "qsv":
		filters = append(filters, "format=nv12")
	}
	if len(filters) > 0 {
		args = append(args, "-vf", strings.Join(filters, ","))
	}
	args = append(args, "-c:v", videoEncoder(backend, p.Codec))
	q := fmt.Sprint(p.Quality)
	switch backend {
	case "cpu":
		args = append(args, "-crf", q, "-preset", "medium")
	case "vaapi":
		args = append(args, "-qp", q)
	case "nvenc":
		args = append(args, "-cq", q, "-b:v", "0", "-preset", "p4")
	case "qsv":
		args = append(args, "-global_quality", q)
	}
	if backend == "cpu" || backend == "nvenc" {
		args = append(args, "-pix_fmt", "yuv420p")
	}
	args = append(args, "-c:a", "aac", "-b:a", "128k", "-movflags", "+faststart", "-f", "mp4", dest)
	return args
}
func videoDimensions(ctx context.Context, path string) (int, int, error) {
	probe, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0, 0, err
	}
	d, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(d, probe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=width,height", "-of", "json", path).Output()
	if err != nil {
		return 0, 0, err
	}
	var v struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	if err = json.Unmarshal(out, &v); err != nil {
		return 0, 0, err
	}
	if len(v.Streams) == 0 || v.Streams[0].Width < 2 || v.Streams[0].Height < 2 {
		return 0, 0, errors.New("нет доступного видеопотока")
	}
	return v.Streams[0].Width, v.Streams[0].Height, nil
}

var errVideoNotSmaller = errors.New("версия не стала меньше оригинала; дополнительная копия не сохранена")

func transcodeVideo(ctx context.Context, p VideoPolicy, src, dest string) (string, error) {
	if err := validateVideoPolicy(p); err != nil {
		return "", err
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", errors.New("FFmpeg отсутствует; установите пакет ffmpeg")
	}
	width, height, err := videoDimensions(ctx, src)
	if err != nil {
		return "", err
	}
	d := probeVideo(ctx, p.Codec, true)
	var failures []string
	for _, backend := range preferredBackends(p, d) {
		if !backend.Available {
			failures = append(failures, backend.Name+": "+backend.Reason)
			continue
		}
		temp := dest + ".part.mp4"
		_ = os.Remove(temp)
		work, cancel := context.WithTimeout(ctx, 45*time.Minute)
		command := exec.CommandContext(work, ffmpeg, buildVideoArgs(p, backend.Name, backend.Device, src, temp, width, height)...)
		out, runErr := command.CombinedOutput()
		cancel()
		if runErr == nil {
			if stat, e := os.Stat(temp); e == nil && stat.Size() > 0 {
				if p.OnlySmaller {
					if original, e := os.Stat(src); e == nil && stat.Size() >= original.Size() {
						_ = os.Remove(temp)
						return backend.Name, errVideoNotSmaller
					}
				}
				if err = os.Rename(temp, dest); err == nil {
					return backend.Name, nil
				}
			} else if runErr == nil {
				runErr = errors.New("FFmpeg создал пустой файл")
			}
		}
		_ = os.Remove(temp)
		msg := strings.TrimSpace(string(out))
		if len(msg) > 240 {
			msg = msg[:240]
		}
		if msg == "" {
			msg = fmt.Sprint(runErr)
		}
		failures = append(failures, backend.Name+": "+msg)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return "", errors.New(strings.Join(failures, "; "))
}

func (a *App) setVideoStatus(key string, v VideoJobStatus) {
	a.videoMu.Lock()
	defer a.videoMu.Unlock()
	if a.videoStatuses == nil {
		a.videoStatuses = make(map[string]VideoJobStatus)
	}
	if len(a.videoStatuses) > 100 {
		for k := range a.videoStatuses {
			delete(a.videoStatuses, k)
			break
		}
	}
	v.UpdatedAt = time.Now().UTC()
	a.videoStatuses[key] = v
}
func (a *App) getVideoStatuses() map[string]VideoJobStatus {
	a.videoMu.Lock()
	defer a.videoMu.Unlock()
	out := map[string]VideoJobStatus{}
	for k, v := range a.videoStatuses {
		out[k] = v
	}
	return out
}
func (a *App) videoAdmin(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		fail(w, 403, "только администратор")
		return
	}
	p := a.accounts.policy().Video
	// Quick inspection is automatic; active encoding only runs on request.
	probe := r.URL.Query().Get("probe") == "1"
	diag := probeVideo(r.Context(), p.Codec, probe)
	hardware := inspectHardware(r.Context(), diag, p.Codec, probe)
	reply(w, 200, map[string]any{"policy": p, "diagnostics": diag, "hardware": hardware, "jobs": a.getVideoStatuses(), "archives": a.videoArchives()})
}
func (a *App) videoEnqueue(w http.ResponseWriter, r *http.Request) {
	if !principal(r).Admin {
		fail(w, 403, "только администратор")
		return
	}
	if !a.accounts.policy().Video.Enabled {
		fail(w, 409, "включите обработку видео в настройках администратора")
		return
	}
	j, ok := a.store.get(r.PathValue("id"))
	if !ok || (j.State != "done" && j.State != "partial") {
		fail(w, 404, "архив не найден или не завершён")
		return
	}
	var req struct {
		Filename string `json:"filename"`
	}
	if decodeRequest(w, r, &req) != nil {
		return
	}
	if req.Filename != "" && !safeArchivePart(req.Filename) {
		fail(w, 400, "неверное имя медиафайла")
		return
	}
	task := VideoTask{JobID: j.ID, Filename: req.Filename, Force: true}
	select {
	case a.videoQueue <- task:
		reply(w, 202, map[string]bool{"queued": true})
	default:
		fail(w, 429, "очередь кодирования заполнена")
	}
}
func (a *App) videoWorker() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	// First pass restores interrupted work after a service restart.
	a.scanVideoJobs()
	for {
		select {
		case <-a.ctx.Done():
			return
		case t := <-a.videoQueue:
			a.processVideoTask(t)
		case <-ticker.C:
			a.scanVideoJobs()
		}
	}
}
func (a *App) scanVideoJobs() {
	policy := a.accounts.policy().Video
	if !policy.Enabled || !policy.Automatic {
		return
	}
	for _, job := range a.store.all() {
		if a.ctx.Err() != nil {
			return
		}
		if job.State == "done" || job.State == "partial" {
			a.processVideoTask(VideoTask{JobID: job.ID})
		}
	}
}
func (a *App) processVideoTask(task VideoTask) {
	policy := a.accounts.policy().Video
	if !policy.Enabled {
		return
	}
	job, ok := a.store.get(task.JobID)
	if !ok || (job.State != "done" && job.State != "partial") {
		return
	}
	folder := a.store.archiveDir(job.OwnerID, job.Author, job.PostID)
	b, err := os.ReadFile(filepath.Join(folder, "manifest.json"))
	if err != nil {
		return
	}
	var man ArchiveManifest
	if json.Unmarshal(b, &man) != nil {
		return
	}
	for _, original := range man.Assets {
		if a.ctx.Err() != nil {
			return
		}
		if original.Type != "ok_video" || task.Filename != "" && original.Filename != task.Filename {
			continue
		}
		if !safeArchivePart(original.Filename) {
			continue
		}
		key := task.JobID + "/" + original.Filename
		targetName := original.Filename + "." + policy.Codec + ".mp4"
		dest := filepath.Join(folder, "media", targetName)
		matched := false
		for _, asset := range man.Assets {
			if asset.Filename == targetName && asset.Type == "transcoded_video" && asset.SourceSHA256 == original.SHA256 && asset.SHA256 != "" {
				if valid, _ := verifyAsset(dest, asset); valid {
					matched = true
				}
				break
			}
		}
		if matched {
			a.setVideoStatus(key, VideoJobStatus{Filename: original.Filename, State: "done", Message: "Готовая версия уже сохранена"})
			continue
		}
		if !task.Force && a.videoRecentlyFailed(key) {
			continue
		}
		source := filepath.Join(folder, "media", original.Filename)
		if info, err := os.Stat(source); err != nil || !info.Mode().IsRegular() {
			a.setVideoStatus(key, VideoJobStatus{Filename: original.Filename, State: "failed", Message: "Исходное медиа отсутствует"})
			a.videoMarkFailure(key)
			continue
		}
		a.setVideoStatus(key, VideoJobStatus{Filename: original.Filename, State: "running"})
		backend, err := transcodeVideo(a.ctx, policy, source, dest)
		if err != nil {
			status := "failed"
			if errors.Is(err, errVideoNotSmaller) {
				status = "skipped"
			}
			a.setVideoStatus(key, VideoJobStatus{Filename: original.Filename, State: status, Backend: backend, Message: err.Error()})
			a.videoMarkFailure(key)
			continue
		}
		size, digest, err := fileDigest(dest)
		if err != nil {
			a.setVideoStatus(key, VideoJobStatus{Filename: original.Filename, State: "failed", Message: err.Error()})
			a.videoMarkFailure(key)
			continue
		}
		// Another archive refresh can occur while FFmpeg is running. Never attach a
		// rendition to a different revision or overwrite a concurrently saved manifest.
		currentJob, exists := a.store.get(job.ID)
		if !exists || currentJob.State == "running" || currentJob.State == "queued" {
			_ = os.Remove(dest)
			continue
		}
		latest, err := os.ReadFile(filepath.Join(folder, "manifest.json"))
		if err != nil {
			continue
		}
		var updated ArchiveManifest
		if json.Unmarshal(latest, &updated) != nil {
			continue
		}
		stillCurrent := false
		for _, asset := range updated.Assets {
			if asset.Filename == original.Filename && asset.SHA256 == original.SHA256 {
				stillCurrent = true
				break
			}
		}
		if !stillCurrent {
			_ = os.Remove(dest)
			continue
		}
		filtered := make([]ArchiveAsset, 0, len(updated.Assets)+1)
		for _, asset := range updated.Assets {
			if asset.Type == "transcoded_video" && asset.SourceFile == original.Filename {
				continue
			}
			filtered = append(filtered, asset)
		}
		filtered = append(filtered, ArchiveAsset{Filename: targetName, Type: "transcoded_video", Title: original.Title, Size: size, SHA256: digest, SourceFile: original.Filename, SourceSHA256: original.SHA256})
		updated.Assets = filtered
		if err = writeJSON(filepath.Join(folder, "manifest.json"), updated); err != nil {
			a.setVideoStatus(key, VideoJobStatus{Filename: original.Filename, State: "failed", Message: err.Error()})
			continue
		}
		// The original remains accessible even if rebuilding the presentation fails.
		_ = rebuildVideoArchiveHTML(folder, updated)
		a.setVideoStatus(key, VideoJobStatus{Filename: original.Filename, State: "done", Backend: backend, Message: "Сохранена дополнительная версия; исходник не изменён"})
	}
}
func (a *App) videoRecentlyFailed(key string) bool {
	a.videoMu.Lock()
	defer a.videoMu.Unlock()
	return time.Since(a.videoFailures[key]) < 24*time.Hour
}
func (a *App) videoMarkFailure(key string) {
	a.videoMu.Lock()
	defer a.videoMu.Unlock()
	if a.videoFailures == nil {
		a.videoFailures = map[string]time.Time{}
	}
	a.videoFailures[key] = time.Now()
}

// Offline HTML can be refreshed without fetching private Boosty URLs.
func readSavedJSON(base string, dst any) error {
	raw, err := os.ReadFile(filepath.Join(base))
	if err == nil {
		return json.Unmarshal(raw, dst)
	}
	if !os.IsNotExist(err) {
		return err
	}
	compressed := base + ".zst"
	stat, err := os.Stat(compressed)
	if err != nil {
		return err
	}
	if stat.Size() > 32<<20 {
		return errors.New("слишком большой сохранённый JSON")
	}
	command := exec.Command("zstd", "-q", "-dc", compressed)
	reader, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err = command.Start(); err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(reader, 64<<20+1))
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return err
	}
	if len(b) > 64<<20 {
		_ = command.Process.Kill()
		_ = command.Wait()
		return errors.New("сохранённый JSON превышает 64 МБ")
	}
	if err = command.Wait(); err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}
func rebuildVideoArchiveHTML(folder string, man ArchiveManifest) error {
	var p struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := readSavedJSON(filepath.Join(folder, "post.json"), &p); err != nil {
		return err
	}
	// Source content blocks only need Data; tokens are never passed to the HTML renderer.
	post := &boosty.Post{Data: p.Data}
	var comments boosty.CommentResult
	_ = readSavedJSON(filepath.Join(folder, "comments.json"), &comments)
	var cv []commentView
	appendComments(&cv, comments.Comments, 0)
	var out bytes.Buffer
	if err := pageTemplate.Execute(&out, renderData{ArchiveManifest: man, Blocks: archiveContentBlocks(post, man.Assets), Comments: cv}); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(folder, "index.html"), out.Bytes())
}

// The inventory is administrator-only, capped to keep the settings page fast.
type videoArchiveRow struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Author string   `json:"author"`
	Owner  string   `json:"owner"`
	Files  []string `json:"files"`
}

func (a *App) videoArchives() []videoArchiveRow {
	out := make([]videoArchiveRow, 0)
	for _, j := range a.store.all() {
		if len(out) >= 100 {
			break
		}
		if j.State != "done" && j.State != "partial" {
			continue
		}
		b, e := os.ReadFile(filepath.Join(a.store.archiveDir(j.OwnerID, j.Author, j.PostID), "manifest.json"))
		if e != nil {
			continue
		}
		var man ArchiveManifest
		if json.Unmarshal(b, &man) != nil {
			continue
		}
		row := videoArchiveRow{ID: j.ID, Title: j.Title, Author: j.Author, Owner: a.accounts.username(j.OwnerID), Files: []string{}}
		for _, m := range man.Assets {
			if m.Type == "ok_video" && safeArchivePart(m.Filename) {
				row.Files = append(row.Files, m.Filename)
			}
		}
		if len(row.Files) > 0 {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out
}

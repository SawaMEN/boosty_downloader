package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVideoPolicyValidationAndPersistence(t *testing.T) {
	app, e := New(Config{DataDir: t.TempDir(), AdminUser: "admin", AdminPassword: "long-password-123", SecretKey: testKey(t), SyncInterval: 24 * time.Hour})
	if e != nil {
		t.Fatal(e)
	}
	admin := testLogin(t, app, "admin", "long-password-123")
	member := testMember(t, app, "membervid")
	if p := app.accounts.policy().Video; p.Enabled || p.Automatic || p.Codec != "h264" || p.Backend != "auto" || !p.FallbackCPU {
		t.Fatalf("default video policy: %+v", p)
	}
	policyRequest(t, app, member, "GET", "/api/admin/video", "", 403)
	policyRequest(t, app, member, "POST", "/api/admin/video/jobs/xyz", `{"filename":"video.mp4"}`, 403)
	p := app.accounts.policy()
	p.Video.Enabled = true
	p.Video.Automatic = true
	p.Video.Backend = "vaapi"
	p.Video.MaxHeight = 720
	if e = app.accounts.changePolicy(p); e != nil {
		t.Fatal(e)
	}
	diag := policyRequest(t, app, admin, "GET", "/api/admin/video", "", 200)
	if !strings.Contains(string(diag), `"renderDevices"`) {
		t.Fatal("missing hardware diagnostics")
	}
	for _, candidate := range []struct{ change func(*VideoPolicy) }{{func(p *VideoPolicy) { p.Backend = "shell" }}, {func(p *VideoPolicy) { p.Codec = "raw" }}, {func(p *VideoPolicy) { p.Quality = 80 }}, {func(p *VideoPolicy) { p.MaxHeight = 9000 }}} {
		bad := p
		candidate.change(&bad.Video)
		if e = validatePolicy(bad); e == nil {
			t.Fatalf("accepted invalid video policy %+v", bad.Video)
		}
	}
	app.Close()
	other, e := New(Config{DataDir: app.cfg.DataDir, AdminUser: "admin", AdminPassword: "long-password-123", SecretKey: testKey(t), SyncInterval: 24 * time.Hour})
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	saved := other.accounts.policy().Video
	if !saved.Enabled || !saved.Automatic || saved.Backend != "vaapi" || saved.MaxHeight != 720 {
		t.Fatalf("video settings not persisted: %+v", saved)
	}
}
func TestVideoEncoderArguments(t *testing.T) {
	p := defaultVideoPolicy()
	p.Enabled = true
	p.MaxHeight = 720
	args := buildVideoArgs(p, "vaapi", "/dev/dri/renderD128", "/archive/input.mkv", "/archive/out.part.mp4", 1920, 1080)
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "-vaapi_device /dev/dri/renderD128") || !strings.Contains(joined, "-vf scale=1280:720,format=nv12,hwupload") || !strings.Contains(joined, "h264_vaapi") {
		t.Fatal(joined)
	}
	cpu := strings.Join(buildVideoArgs(p, "cpu", "", "/archive/input.mkv", "/archive/out.part.mp4", 720, 480), " ")
	if !strings.Contains(cpu, "libx264") || strings.Contains(cpu, "hwupload") {
		t.Fatal(cpu)
	}
	if got := videoEncoder("nvenc", "hevc"); got != "hevc_nvenc" {
		t.Fatal(got)
	}
}
func TestCPUTranscodeProducesIndependentOriginal(t *testing.T) {
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Skip("FFmpeg not installed")
	}
	if _, e := exec.LookPath("ffprobe"); e != nil {
		t.Skip("FFprobe not installed")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "source.mkv")
	dest := filepath.Join(dir, "copy.mp4")
	cmd := exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=640x480:rate=10", "-t", "0.6", "-c:v", "mpeg4", src)
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("fixture: %s: %v", b, e)
	}
	original, e := os.ReadFile(src)
	if e != nil {
		t.Fatal(e)
	}
	p := defaultVideoPolicy()
	p.Enabled = true
	p.Backend = "cpu"
	p.OnlySmaller = false
	p.MaxHeight = 240
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	backend, e := transcodeVideo(ctx, p, src, dest)
	if e != nil {
		t.Fatal(e)
	}
	if backend != "cpu" {
		t.Fatal(backend)
	}
	width, height, e := videoDimensions(ctx, dest)
	if e != nil {
		t.Fatal(e)
	}
	if width != 320 || height != 240 {
		t.Fatalf("incorrect dimensions %dx%d", width, height)
	}
	after, e := os.ReadFile(src)
	if e != nil {
		t.Fatal(e)
	}
	if string(after) != string(original) {
		t.Fatal("original was modified")
	}
	if _, e := os.Stat(dest + ".part.mp4"); !os.IsNotExist(e) {
		t.Fatal("left temporary video")
	}
}
func TestVideoWorkerArchivesRendition(t *testing.T) {
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		t.Skip("ffmpeg missing")
	}
	app, e := New(Config{DataDir: t.TempDir(), AdminUser: "admin", AdminPassword: "long-password-123", SecretKey: testKey(t), SyncInterval: 24 * time.Hour})
	if e != nil {
		t.Fatal(e)
	}
	defer app.Close()
	p := app.accounts.policy()
	p.Video.Enabled = true
	p.Video.Backend = "cpu"
	p.Video.OnlySmaller = false
	p.Video.Automatic = false
	if e = app.accounts.changePolicy(p); e != nil {
		t.Fatal(e)
	}
	job := Job{ID: "video-work", OwnerID: "admin", Author: "somebody", PostID: "abc123", State: "done", Title: "video archive"}
	if e = app.store.upsert(job); e != nil {
		t.Fatal(e)
	}
	base := app.store.archiveDir(job.OwnerID, job.Author, job.PostID)
	if e = os.MkdirAll(filepath.Join(base, "media"), 0700); e != nil {
		t.Fatal(e)
	}
	src := filepath.Join(base, "media", "source.mkv")
	cmd := exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=10", "-t", "0.3", "-c:v", "mpeg4", src)
	if b, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("fixture: %s %v", b, e)
	}
	n, digest, e := fileDigest(src)
	if e != nil {
		t.Fatal(e)
	}
	man := ArchiveManifest{Title: "Video", Assets: []ArchiveAsset{{Filename: "source.mkv", Type: "ok_video", Size: n, SHA256: digest}}}
	if e = writeJSON(filepath.Join(base, "manifest.json"), man); e != nil {
		t.Fatal(e)
	}
	app.processVideoTask(VideoTask{JobID: job.ID})
	b, e := os.ReadFile(filepath.Join(base, "manifest.json"))
	if e != nil {
		t.Fatal(e)
	}
	var output ArchiveManifest
	if e = json.Unmarshal(b, &output); e != nil {
		t.Fatal(e)
	}
	if len(output.Assets) != 2 {
		t.Fatalf("no rendition generated: %+v, states %+v", output.Assets, app.getVideoStatuses())
	}
	generated := output.Assets[1]
	if generated.SourceFile != "source.mkv" || generated.SourceSHA256 != digest || generated.Type != "transcoded_video" {
		t.Fatalf("bad rendition: %+v", generated)
	}
	valid, e := verifyAsset(filepath.Join(base, "media", generated.Filename), generated)
	if e != nil || !valid {
		t.Fatalf("invalid rendition: %v, %v", valid, e)
	}
	app.processVideoTask(VideoTask{JobID: job.ID})
	b, e = os.ReadFile(filepath.Join(base, "manifest.json"))
	if e != nil {
		t.Fatal(e)
	}
	if json.Unmarshal(b, &output) != nil || len(output.Assets) != 2 {
		t.Fatal("duplicated rendition")
	}
	if _, e = os.Stat(src); e != nil {
		t.Fatal("original missing", e)
	}
	policyRequest(t, app, testLogin(t, app, "admin", "long-password-123"), "POST", "/api/admin/video/jobs/video-work", `{"filename":"../escape"}`, http.StatusBadRequest)
}

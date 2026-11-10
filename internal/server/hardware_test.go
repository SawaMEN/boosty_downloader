package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHardwareCPUInstructionsFromKernelFlags(t *testing.T) {
	cpu := parseCPUInfo("processor : 0\nmodel name : AMD Ryzen 5 5600H\nflags : fpu sse4_2 avx avx2 fma bmi2 aes sha_ni\n", "amd64", 12)
	if cpu.Model != "AMD Ryzen 5 5600H" || cpu.LogicalCores != 12 {
		t.Fatalf("cpu info %+v", cpu)
	}
	for _, instruction := range []string{"AVX2", "FMA", "AES", "SHA", "BMI2"} {
		if !strings.Contains(strings.Join(cpu.Instructions, ","), instruction) {
			t.Fatalf("missing %s: %+v", instruction, cpu.Instructions)
		}
	}
	if strings.Contains(strings.Join(cpu.Instructions, ","), "AVX-512") {
		t.Fatal("invented AVX-512 on Ryzen 5600H")
	}
	mixed := parseCPUInfo("processor : 0\nflags : avx avx2 aes\nprocessor : 1\nflags : avx aes\n", "amd64", 2)
	if strings.Contains(strings.Join(mixed.Instructions, ","), "AVX2") {
		t.Fatal("instruction only on one core must not be advertised as universally available")
	}
	arm := parseCPUInfo("processor : 0\nFeatures : fp asimd aes sha2 sve\n", "arm64", 8)
	for _, instruction := range []string{"NEON/ASIMD", "ARM SHA2", "SVE"} {
		if !strings.Contains(strings.Join(arm.Instructions, ","), instruction) {
			t.Fatalf("missing %s: %+v", instruction, arm.Instructions)
		}
	}
}

func TestHardwareVAProfilesAndCodecAdvertisements(t *testing.T) {
	profiles := parseVAProfiles(`VAProfileH264High : VAEntrypointVLD
VAProfileH264High : VAEntrypointEncSlice
VAProfileHEVCMain : VAEntrypointVLD
VAProfileAV1Profile0 : VAEntrypointVLD`)
	text := strings.Join(profiles, ",")
	for _, want := range []string{"H.264 decode", "H.264 encode", "HEVC decode", "AV1 decode"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing advertised VA profile %s: %s", want, text)
		}
	}
	if strings.Contains(text, "AV1 encode") || strings.Contains(text, "HEVC encode") {
		t.Fatal("inferred unsupported encode profile")
	}
	listing := []byte(" V....D libx264 libx264 H.264\n V....D h264_vaapi VAAPI H.264\n V....D hevc_nvenc NVIDIA HEVC\n")
	if !hasEncoder(listing, "libx264") || !hasEncoder(listing, "h264_vaapi") || !hasEncoder(listing, "hevc_nvenc") {
		t.Fatalf("codec listing parser failed")
	}
	if hasEncoder(listing, "h264_nvenc") || hasEncoder(listing, "libx265") {
		t.Fatal("inferred unlisted codec")
	}
	rows := hardwareCodecs(listing, VideoDiagnostics{Backends: []VideoBackend{{Name: "vaapi", Available: true}}}, "h264", true)
	for _, c := range rows {
		if c.Backend == "vaapi" && c.Codec == "h264" && !c.Verified {
			t.Fatal("hardware encode test not respected")
		}
		if c.Backend == "nvenc" && c.Codec == "hevc" && c.Verified {
			t.Fatal("unprobed HEVC must not be verified")
		}
	}
}

func TestHardwareRecommendationsRequireRealEncoderProbe(t *testing.T) {
	r := HardwareReport{
		Distribution: "ubuntu", Container: false, DeepProbe: false,
		GPUs:   []HardwareGPU{{Vendor: "AMD Radeon", Device: "/dev/dri/renderD128", Accessible: true}},
		Tools:  map[string]bool{"ffmpeg": true, "ffprobe": true, "vainfo": false},
		Codecs: []HardwareCodec{{Backend: "vaapi", Codec: "h264", Built: true}},
	}
	profile, suggestions := makeHardwareAdvice(r)
	if profile.Verified || profile.Backend != "auto" {
		t.Fatalf("must not recommend unverified backend: %+v", profile)
	}
	if len(suggestions) == 0 {
		t.Fatal("missing package advice")
	}
	r.DeepProbe = true
	r.Codecs[0].Verified = true
	profile, _ = makeHardwareAdvice(r)
	if !profile.Verified || profile.Backend != "vaapi" || profile.Codec != "h264" {
		t.Fatalf("AMD must recommend validated VAAPI: %+v", profile)
	}
	r.GPUs = []HardwareGPU{{Vendor: "Intel", Device: "/dev/dri/renderD129", Accessible: true}}
	r.Codecs = append(r.Codecs, HardwareCodec{Backend: "qsv", Codec: "h264", Built: true, Verified: true})
	profile, _ = makeHardwareAdvice(r)
	if profile.Backend != "qsv" {
		t.Fatalf("Intel should prefer QSV: %+v", profile)
	}
}

func TestHardwareInventoryIsAdminOnlyAndReadOnly(t *testing.T) {
	app, err := New(Config{DataDir: t.TempDir(), AdminUser: "admin", AdminPassword: "long-password-123", SecretKey: testKey(t), SyncInterval: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	admin := testLogin(t, app, "admin", "long-password-123")
	member := testMember(t, app, "hwguest")
	policyRequest(t, app, member, "GET", "/api/admin/video?probe=1", "", http.StatusForbidden)
	data := policyRequest(t, app, admin, "GET", "/api/admin/video", "", http.StatusOK)
	var response struct {
		Hardware HardwareReport `json:"hardware"`
	}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Hardware.System == "" || response.Hardware.CPU.LogicalCores < 1 || len(response.Hardware.Codecs) != 8 || len(response.Hardware.Tools) == 0 {
		t.Fatalf("missing expected hardware inventory: %+v", response.Hardware)
	}
	if response.Hardware.DeepProbe {
		t.Fatal("inventory must not launch active hardware probe without explicit admin action")
	}
	if app.accounts.policy().Video.Enabled {
		t.Fatal("inventory must not enable video processing")
	}
}

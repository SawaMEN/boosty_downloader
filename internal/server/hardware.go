package server

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Hardware inventory is administrative metadata only. It never changes kernel,
// drivers or packages; enabling an accelerator always requires administrator
// confirmation and a successful real encoder probe.
type HardwareCPU struct {
	Model        string   `json:"model"`
	Architecture string   `json:"architecture"`
	LogicalCores int      `json:"logicalCores"`
	Instructions []string `json:"instructions"`
	Notes        []string `json:"notes"`
}

type HardwareGPU struct {
	Vendor     string   `json:"vendor"`
	Device     string   `json:"device"`
	PCI        string   `json:"pci,omitempty"`
	Driver     string   `json:"driver,omitempty"`
	Accessible bool     `json:"accessible"`
	Profiles   []string `json:"profiles"`
	Issue      string   `json:"issue,omitempty"`
}

type HardwareCodec struct {
	Backend  string `json:"backend"`
	Codec    string `json:"codec"`
	Encoder  string `json:"encoder"`
	Built    bool   `json:"built"`
	Verified bool   `json:"verified"`
	Reason   string `json:"reason,omitempty"`
}

type HardwareSuggestion struct {
	ID       string `json:"id"`
	Priority string `json:"priority"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Command  string `json:"command,omitempty"`
}

type HardwareProfile struct {
	Backend     string `json:"backend"`
	Codec       string `json:"codec"`
	Verified    bool   `json:"verified"`
	Explanation string `json:"explanation"`
}

type HardwareReport struct {
	CheckedAt    time.Time            `json:"checkedAt"`
	System       string               `json:"system"`
	Distribution string               `json:"distribution"`
	Container    bool                 `json:"container"`
	CPU          HardwareCPU          `json:"cpu"`
	GPUs         []HardwareGPU        `json:"gpus"`
	Tools        map[string]bool      `json:"tools"`
	Codecs       []HardwareCodec      `json:"codecs"`
	Accelerators []string             `json:"accelerators"`
	Profile      HardwareProfile      `json:"profile"`
	Suggestions  []HardwareSuggestion `json:"suggestions"`
	DeepProbe    bool                 `json:"deepProbe"`
}

var usefulCPUInstructions = []struct {
	Name string
	Flag string
}{
	{"SSE4.2", "sse4_2"},
	{"AVX", "avx"},
	{"AVX2", "avx2"},
	{"AVX-512", "avx512f"},
	{"FMA", "fma"},
	{"BMI2", "bmi2"},
	{"AES", "aes"},
	{"SHA", "sha_ni"},
	{"NEON/ASIMD", "asimd"},
	{"ARM SHA2", "sha2"},
	{"SVE", "sve"},
}

func parseCPUInfo(contents string, arch string, logicalCores int) HardwareCPU {
	cpu := HardwareCPU{Architecture: arch, LogicalCores: logicalCores, Instructions: []string{}, Notes: []string{}}
	flags := map[string]bool{}
	seenFlags := false
	scanner := bufio.NewScanner(strings.NewReader(contents))
	for scanner.Scan() {
		key, val, found := strings.Cut(scanner.Text(), ":")
		if !found {
			continue
		}
		key, val = strings.ToLower(strings.TrimSpace(key)), strings.TrimSpace(val)
		switch key {
		case "model name", "hardware", "processor":
			if cpu.Model == "" && val != "" && (key != "processor" || !isDigits(val)) {
				cpu.Model = val
			}
		case "flags", "features":
			core := make(map[string]bool)
			for _, flag := range strings.Fields(strings.ToLower(val)) {
				core[flag] = true
			}
			if !seenFlags {
				flags = core
				seenFlags = true
			} else {
				// Heterogeneous systems must not advertise an instruction
				// supported by only some of the schedulable cores.
				for flag := range flags {
					if !core[flag] {
						delete(flags, flag)
					}
				}
			}
		}
	}
	if cpu.Model == "" {
		cpu.Model = "Модель не определена"
	}
	for _, item := range usefulCPUInstructions {
		if flags[item.Flag] {
			cpu.Instructions = append(cpu.Instructions, item.Name)
		}
	}
	if flags["avx2"] && flags["fma"] {
		cpu.Notes = append(cpu.Notes, "AVX2 и FMA доступны процессу: оптимизированные сборки кодеков могут их использовать")
	}
	if flags["aes"] {
		cpu.Notes = append(cpu.Notes, "Доступно аппаратное ускорение операций AES, если его использует установленная библиотека")
	}
	if len(cpu.Instructions) == 0 {
		cpu.Notes = append(cpu.Notes, "Инструкции не определены; на не-Linux системах или в контейнере список может быть неполным")
	}
	return cpu
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func detectGPU() []HardwareGPU {
	out := []HardwareGPU{}
	for _, node := range renderDevices() {
		base := filepath.Base(node)
		info := filepath.Join("/sys/class/drm", base, "device")
		vendor, _ := os.ReadFile(filepath.Join(info, "vendor"))
		device, _ := os.ReadFile(filepath.Join(info, "device"))
		gpu := HardwareGPU{Device: node, PCI: strings.TrimSpace(string(device)), Profiles: []string{}}
		switch strings.ToLower(strings.TrimSpace(string(vendor))) {
		case "0x1002":
			gpu.Vendor = "AMD Radeon"
		case "0x8086":
			gpu.Vendor = "Intel"
		case "0x10de":
			gpu.Vendor = "NVIDIA"
		default:
			gpu.Vendor = "Неизвестный GPU"
		}
		if driver, err := filepath.EvalSymlinks(filepath.Join(info, "driver")); err == nil {
			gpu.Driver = filepath.Base(driver)
		}
		if file, err := os.OpenFile(node, os.O_RDWR, 0); err == nil {
			gpu.Accessible = true
			_ = file.Close()
		} else {
			gpu.Issue = "Нет доступа к устройству: проверьте права render/video и проброс Docker"
		}
		out = append(out, gpu)
	}
	if _, err := os.Stat("/dev/nvidia0"); err == nil {
		seen := false
		for _, gpu := range out {
			seen = seen || gpu.Vendor == "NVIDIA"
		}
		if !seen {
			gpu := HardwareGPU{Vendor: "NVIDIA", Device: "/dev/nvidia0", Profiles: []string{}}
			if file, e := os.OpenFile(gpu.Device, os.O_RDWR, 0); e == nil {
				gpu.Accessible = true
				_ = file.Close()
			} else {
				gpu.Issue = "Нет доступа к /dev/nvidia0"
			}
			out = append(out, gpu)
		}
	}
	return out
}

// VA-API profiles are advertised driver features, not proof that FFmpeg can
// decode or encode a particular file. The encoder probe is the final check.
func queryVAProfiles(ctx context.Context, node string) []string {
	tool, err := exec.LookPath("vainfo")
	if err != nil {
		return []string{}
	}
	check, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(check, tool, "--display", "drm", "--device", node).CombinedOutput()
	if err != nil {
		return []string{}
	}
	return parseVAProfiles(string(output))
}

func parseVAProfiles(output string) []string {
	profiles := map[string]bool{}
	for _, row := range strings.Split(output, "\n") {
		codec := ""
		for _, item := range []struct{ Match, Codec string }{
			{"VAProfileH264", "H.264"}, {"VAProfileHEVC", "HEVC"},
			{"VAProfileVP9", "VP9"}, {"VAProfileAV1", "AV1"},
			{"VAProfileJPEGBaseline", "JPEG"}, {"VAProfileVP8", "VP8"},
		} {
			if strings.Contains(row, item.Match) {
				codec = item.Codec
				break
			}
		}
		if codec == "" {
			continue
		}
		if strings.Contains(row, "VAEntrypointVLD") {
			profiles[codec+" decode"] = true
		} else if strings.Contains(row, "VAEntrypointEncSlice") || strings.Contains(row, "VAEntrypointEncPicture") {
			profiles[codec+" encode"] = true
		}
	}
	result := make([]string, 0, len(profiles))
	for p := range profiles {
		result = append(result, p)
	}
	sort.Strings(result)
	return result
}

func detectDistribution() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return runtime.GOOS
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "ID=") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "ID=")), "\"")
		}
	}
	return runtime.GOOS
}

func runningInContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	for _, path := range []string{"/proc/1/cgroup", "/proc/self/cgroup"} {
		data, err := os.ReadFile(path)
		if err == nil && (bytes.Contains(data, []byte("docker")) || bytes.Contains(data, []byte("kubepods")) || bytes.Contains(data, []byte("containerd"))) {
			return true
		}
	}
	return false
}

func ffmpegEncoderList(ctx context.Context) []byte {
	binary, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil
	}
	check, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(check, binary, "-hide_banner", "-encoders").Output()
	return out
}

// FFmpeg -hwaccels lists compiled interfaces, not successfully opened GPU
// devices or functioning decoders. Always label this as an advertisement.
func ffmpegAccelerators(ctx context.Context) []string {
	binary, err := exec.LookPath("ffmpeg")
	if err != nil {
		return []string{}
	}
	check, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(check, binary, "-hide_banner", "-hwaccels").Output()
	if err != nil {
		return []string{}
	}
	list := []string{}
	for _, row := range strings.Split(string(output), "\n") {
		name := strings.TrimSpace(row)
		if name == "" || strings.Contains(name, " ") || strings.Contains(name, ":") {
			continue
		}
		switch name {
		case "vaapi", "cuda", "qsv", "vdpau", "drm", "vulkan", "opencl", "videotoolbox", "d3d11va", "dxva2", "amf":
			list = append(list, name)
		}
	}
	return list
}

func hasEncoder(list []byte, encoder string) bool {
	for _, line := range strings.Split(string(list), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == encoder && len(fields[0]) >= 6 && strings.ContainsAny(fields[0][:1], "VAS") {
			return true
		}
	}
	return false
}

func hardwareCodecs(list []byte, diagnostic VideoDiagnostics, codec string, active bool) []HardwareCodec {
	out := make([]HardwareCodec, 0, 8)
	for _, backend := range []string{"cpu", "vaapi", "qsv", "nvenc"} {
		for _, name := range []string{"h264", "hevc"} {
			codecRow := HardwareCodec{Backend: backend, Codec: name, Encoder: videoEncoder(backend, name)}
			codecRow.Built = hasEncoder(list, codecRow.Encoder)
			if !codecRow.Built {
				codecRow.Reason = "Кодер отсутствует в FFmpeg"
			} else if name != codec || !active {
				codecRow.Reason = "Обнаружен в FFmpeg; необходим пробный запуск"
			}
			if active && name == codec {
				for _, d := range diagnostic.Backends {
					if d.Name == backend {
						codecRow.Verified = d.Available
						codecRow.Reason = d.Reason
						break
					}
				}
			}
			out = append(out, codecRow)
		}
	}
	return out
}

func makeHardwareAdvice(r HardwareReport) (HardwareProfile, []HardwareSuggestion) {
	suggestions := make([]HardwareSuggestion, 0)
	add := func(id, priority, title, detail, command string) {
		suggestions = append(suggestions, HardwareSuggestion{ID: id, Priority: priority, Title: title, Detail: detail, Command: command})
	}
	debian := r.Distribution == "debian" || r.Distribution == "ubuntu" || r.Distribution == "linuxmint"
	install := func(mode string) string {
		if debian {
			return "sudo bash deploy/boosty-video-deps " + mode
		}
		return "sudo bash deploy/boosty-video-deps " + mode + "  # проверьте доступность пакетов в дистрибутиве"
	}
	if !r.Tools["zstd"] {
		add("zstd", "recommended", "Установить Zstandard для экономии места", "Сжатие JSON-архивов требует системной команды zstd.", "sudo apt-get install -y zstd  # Debian/Ubuntu")
	}
	if !r.Tools["ffmpeg"] || !r.Tools["ffprobe"] {
		add("ffmpeg", "required", "Установить FFmpeg и FFprobe", "Без них недоступны видеодиагностика, просмотр характеристик и перекодирование.", "sudo apt-get install -y ffmpeg  # Debian/Ubuntu; для других систем используйте менеджер пакетов")
	}
	if r.Tools["ffmpeg"] && !r.DeepProbe {
		add("probe", "recommended", "Проверить аппаратный кодер", "Наличие кодера в сборке FFmpeg ещё не гарантирует, что драйвер сможет закодировать видео. Запустите пробу из панели.", "")
	}
	vendorSet := map[string]bool{}
	for _, gpu := range r.GPUs {
		vendorSet[gpu.Vendor] = true
		if !gpu.Accessible {
			add("access-"+filepath.Base(gpu.Device), "required", "Разрешить доступ к "+gpu.Device,
				"Для native проверьте группы render/video пользователя службы. Для Docker добавьте GPU overlay и перезапустите контейнер.", "")
		}
	}
	if vendorSet["AMD Radeon"] {
		if r.Tools["vainfo"] {
			for _, gpu := range r.GPUs {
				if gpu.Vendor == "AMD Radeon" && gpu.Accessible && len(gpu.Profiles) == 0 {
					add("amd-vaapi", "recommended", "Проверить драйвер Mesa VA-API", "GPU обнаружен, но vainfo не вернул профили; проверьте libva, radeonsi, firmware и доступ к устройству.", install("amd"))
					break
				}
			}
		}
		if !r.Tools["vainfo"] {
			add("amd-tools", "recommended", "Установить Mesa VA-API и vainfo", "AMD обычно использует VA-API; нужен рабочий драйвер Mesa и устройство /dev/dri/renderD*.", install("amd"))
		}
	}
	if vendorSet["Intel"] && !r.Tools["vainfo"] {
		add("intel-tools", "recommended", "Установить Intel VA-API и vainfo", "Для Intel QSV нужны драйверы и доступ к /dev/dri.", install("intel"))
	}
	if vendorSet["NVIDIA"] && !r.Tools["nvidia-smi"] {
		add("nvidia-tools", "required", "Проверить драйвер NVIDIA", "Убедитесь, что установлен совместимый драйвер (nvidia-smi); в Docker также необходим NVIDIA Container Toolkit.", "")
	}
	if r.Container && len(r.GPUs) == 0 {
		add("docker-gpu", "optional", "Настроить доступ GPU в Docker", "Если на хосте установлена AMD/Intel/NVIDIA, проверьте соответствующий compose.gpu-*.yaml. До проброса сервера GPU не виден.", "")
	}
	if len(r.GPUs) == 0 && !r.Container {
		add("cpu-only", "optional", "Использовать программное кодирование", "Доступного видеоустройства не обнаружено. CPU-режим не требует аппаратного драйвера.", "")
	}
	best := HardwareProfile{Backend: "auto", Codec: "h264", Explanation: "Автоматический выбор после пробного кодирования. Настройки не изменяются без подтверждения администратора."}
	// Selection only follows an actual successful probe. Declared codecs do not
	// count as working hardware and never trigger automatic hardware activation.
	if r.DeepProbe {
		order := []string{"nvenc", "qsv", "vaapi", "cpu"}
		if vendorSet["AMD Radeon"] {
			order = []string{"vaapi", "nvenc", "qsv", "cpu"}
		} else if vendorSet["Intel"] {
			order = []string{"qsv", "vaapi", "nvenc", "cpu"}
		}
		for _, name := range order {
			for _, c := range r.Codecs {
				if c.Backend == name && c.Verified {
					if name == "cpu" && best.Verified {
						continue
					}
					explanation := "H.264 проверен реальным кодированием. Применение требует сохранения настроек администратором."
					if c.Codec == "hevc" {
						explanation = "HEVC проверен реальным кодированием, но воспроизведение зависит от браузера. Применение требует сохранения настроек администратором."
					}
					best = HardwareProfile{Backend: name, Codec: c.Codec, Verified: true, Explanation: explanation}
					break
				}
			}
			if best.Verified && best.Backend != "cpu" {
				break
			}
		}
		if !best.Verified {
			add("probe-failed", "required", "Кодирование не подтвердилось", "Проверьте причины по каждому кодеру, установите драйвер и повторите диагностику. Не включайте автоматическую обработку до успешной пробы.", "")
		}
	}
	return best, suggestions
}

func inspectHardware(ctx context.Context, diagnostics VideoDiagnostics, codec string, active bool) HardwareReport {
	report := HardwareReport{
		CheckedAt: time.Now().UTC(), System: runtime.GOOS + "/" + runtime.GOARCH,
		Distribution: detectDistribution(), Container: runningInContainer(),
		CPU:  HardwareCPU{Architecture: runtime.GOARCH, LogicalCores: runtime.NumCPU()},
		GPUs: detectGPU(), Tools: map[string]bool{}, DeepProbe: active,
	}
	cpuRaw, err := os.ReadFile("/proc/cpuinfo")
	if err == nil {
		report.CPU = parseCPUInfo(string(cpuRaw), runtime.GOARCH, runtime.NumCPU())
	} else {
		report.CPU = parseCPUInfo("", runtime.GOARCH, runtime.NumCPU())
	}
	for _, bin := range []string{"ffmpeg", "ffprobe", "vainfo", "nvidia-smi", "zstd"} {
		_, err = exec.LookPath(bin)
		report.Tools[bin] = err == nil
	}
	for i := range report.GPUs {
		if report.GPUs[i].Accessible && report.GPUs[i].Device != "/dev/nvidia0" {
			report.GPUs[i].Profiles = queryVAProfiles(ctx, report.GPUs[i].Device)
		}
	}
	report.Codecs = hardwareCodecs(ffmpegEncoderList(ctx), diagnostics, codec, active)
	report.Accelerators = ffmpegAccelerators(ctx)
	report.Profile, report.Suggestions = makeHardwareAdvice(report)
	return report
}

package server

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const adaptiveMemoryReserve = 512 << 20

var mediaExtraSegmentGate = make(chan struct{}, 1)

var adaptiveSegmentSample struct {
	sync.Mutex
	at      time.Time
	workers int
}

func adaptiveSegmentWorkers() int {
	adaptiveSegmentSample.Lock()
	defer adaptiveSegmentSample.Unlock()
	if time.Since(adaptiveSegmentSample.at) >= 5*time.Second || adaptiveSegmentSample.workers == 0 {
		adaptiveSegmentSample.workers = probeSegmentResources()
		adaptiveSegmentSample.at = time.Now()
	}
	return adaptiveSegmentSample.workers
}

func chooseAdaptiveSegmentWorkers(cpus, load float64, available uint64) int {
	if math.IsNaN(cpus) || math.IsInf(cpus, 0) || math.IsNaN(load) || math.IsInf(load, 0) ||
		cpus < 2 || load < 0 || load >= cpus*0.75 || available < adaptiveMemoryReserve {
		return 1
	}
	return 2
}

// Host metrics cannot prove a container's effective limits. Keep automatic
// mode conservative there; administrators can still explicitly opt in.
// On native Linux, inspect the process's unified cgroup and its ancestors.
func probeSegmentResources() int {
	if runtime.GOOS != "linux" || runningInContainer() {
		return 1
	}
	cpus := float64(min(runtime.NumCPU(), runtime.GOMAXPROCS(0)))
	loadRaw, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 1
	}
	loadFields := strings.Fields(string(loadRaw))
	if len(loadFields) == 0 {
		return 1
	}
	load, err := strconv.ParseFloat(loadFields[0], 64)
	if err != nil {
		return 1
	}
	memRaw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 1
	}
	var available uint64
	for _, line := range strings.Split(string(memRaw), "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "MemAvailable:" && f[2] == "kB" {
			kb, err := strconv.ParseUint(f[1], 10, 64)
			if err != nil || kb > ^uint64(0)/1024 {
				return 1
			}
			available = kb * 1024
			break
		}
	}
	groupRaw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 1
	}
	group := ""
	for _, line := range strings.Split(string(groupRaw), "\n") {
		if strings.HasPrefix(line, "0::/") {
			group = strings.TrimPrefix(line, "0::")
		} else if line != "" {
			// Unknown or hybrid controller layout.
			return 1
		}
	}
	if group == "" || filepath.Clean(group) != group {
		return 1
	}
	root := "/sys/fs/cgroup"
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return 1
	}
	for dir := filepath.Join(root, group); dir != root; dir = filepath.Dir(dir) {
		if !strings.HasPrefix(dir, root+"/") {
			return 1
		}
		cpuRaw, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
		if err != nil {
			return 1
		}
		f := strings.Fields(string(cpuRaw))
		if len(f) != 2 {
			return 1
		}
		period, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil || period == 0 {
			return 1
		}
		if f[0] != "max" {
			quota, err := strconv.ParseUint(f[0], 10, 64)
			if err != nil || quota == 0 {
				return 1
			}
			cpus = min(cpus, float64(quota)/float64(period))
		}
		currentRaw, err := os.ReadFile(filepath.Join(dir, "memory.current"))
		if err != nil {
			return 1
		}
		current, err := strconv.ParseUint(strings.TrimSpace(string(currentRaw)), 10, 64)
		if err != nil {
			return 1
		}
		for _, name := range []string{"memory.max", "memory.high"} {
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return 1
			}
			if v := strings.TrimSpace(string(raw)); v != "max" {
				limit, err := strconv.ParseUint(v, 10, 64)
				if err != nil || current >= limit {
					return 1
				}
				available = min(available, limit-current)
			}
		}
	}
	return chooseAdaptiveSegmentWorkers(cpus, load, available)
}

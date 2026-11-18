package collector

import (
	"strconv"
	"strings"
)

// Sample is one system-wide memory reading. All sizes are in kB.
type Sample struct {
	T            int64 `json:"t"` // unix milliseconds (host clock)
	MemTotal     int64 `json:"memTotal"`
	MemAvailable int64 `json:"memAvailable"`
	EffTotal     int64 `json:"effTotal"` // usable RAM: MemTotal - Offline - Balloon
	Offline      int64 `json:"offline"`  // offlined memory blocks still counted in MemTotal (qcom mem-offline)
	Balloon      int64 `json:"balloon"`  // RAM pinned by limit_ram.sh (ramfs), from its state file
	Limit        int64 `json:"limit"`    // limit_ram.sh target (physical size), 0 = no limit
	Used         int64 `json:"used"`     // EffTotal - MemAvailable
	Anon         int64 `json:"anon"`     // AnonPages
	Shmem        int64 `json:"shmem"`    // Shmem
	Kernel       int64 `json:"kernel"`   // SUnreclaim + KernelStack + ShadowCallStack + PageTables + VmallocUsed + Percpu
	Zram         int64 `json:"zram"`     // zram mem_used_total (RAM actually used by the compressed pool)
	Other        int64 `json:"other"`    // Used minus the above: DMA-BUF, GPU, driver pages, non-reclaimable cache...
	ZramOrig     int64 `json:"zramOrig"`
	SwapUsed     int64 `json:"swapUsed"`
}

// parseMeminfo parses /proc/meminfo lines ("Key:   123 kB") into a map of kB values.
func parseMeminfo(lines []string) map[string]int64 {
	m := make(map[string]int64, len(lines))
	for _, l := range lines {
		k, rest, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		m[strings.TrimSpace(k)] = v
	}
	return m
}

// parseMmStat parses /sys/block/zram0/mm_stat (values in bytes).
// Fields: orig_data_size compr_data_size mem_used_total mem_limit mem_used_max same_pages pages_compacted huge_pages
func parseMmStat(s string) (origKB, usedKB int64) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return 0, 0
	}
	orig, _ := strconv.ParseInt(f[0], 10, 64)
	used, _ := strconv.ParseInt(f[2], 10, 64)
	return orig / 1024, used / 1024
}

// parseLimitState parses the state file of limit_ram.sh ("target=4096M hidden=0 balloon=2048", MB values).
func parseLimitState(s string) (targetKB, balloonKB int64) {
	for _, f := range strings.Fields(s) {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimRight(v, "Mm"), 10, 64)
		if err != nil || n < 0 {
			continue
		}
		switch k {
		case "target":
			targetKB = n * 1024
		case "balloon":
			balloonKB = n * 1024
		}
	}
	return targetKB, balloonKB
}

// buildSample turns one reading into a Sample. managedKB is the sum of "managed" pages of all
// zones in /proc/zoneinfo (0 = unknown); limitState is the content of the limit_ram.sh state file.
func buildSample(t int64, mi map[string]int64, mmStat string, managedKB int64, limitState string) Sample {
	s := Sample{T: t, MemTotal: mi["MemTotal"], MemAvailable: mi["MemAvailable"]}
	// Offlined blocks leave the zones but the qcom mem-offline driver keeps them in MemTotal.
	// Small differences are rounding/accounting noise, not offlined memory.
	if managedKB > 0 && s.MemTotal-managedKB > 16*1024 {
		s.Offline = s.MemTotal - managedKB
	}
	s.Limit, s.Balloon = parseLimitState(limitState)
	if s.Limit == 0 {
		s.Balloon = 0 // stale state file without a target: ignore it
	}
	s.EffTotal = s.MemTotal - s.Offline - s.Balloon
	if s.EffTotal <= 0 {
		s.EffTotal = s.MemTotal
	}
	s.Used = s.EffTotal - s.MemAvailable
	if s.Used < 0 {
		s.Used = 0
	}
	s.Anon = mi["AnonPages"]
	s.Shmem = mi["Shmem"]
	s.Kernel = mi["SUnreclaim"] + mi["KernelStack"] + mi["ShadowCallStack"] + mi["PageTables"] + mi["VmallocUsed"] + mi["Percpu"]
	s.ZramOrig, s.Zram = parseMmStat(mmStat)
	s.SwapUsed = mi["SwapTotal"] - mi["SwapFree"]
	s.Other = s.Used - s.Anon - s.Shmem - s.Kernel - s.Zram
	if s.Other < 0 {
		s.Other = 0
	}
	return s
}

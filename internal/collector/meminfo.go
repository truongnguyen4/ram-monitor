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
	Used         int64 `json:"used"`   // MemTotal - MemAvailable
	Anon         int64 `json:"anon"`   // AnonPages
	Shmem        int64 `json:"shmem"`  // Shmem
	Kernel       int64 `json:"kernel"` // SUnreclaim + KernelStack + ShadowCallStack + PageTables + VmallocUsed + Percpu
	Zram         int64 `json:"zram"`   // zram mem_used_total (RAM actually used by the compressed pool)
	Other        int64 `json:"other"`  // Used minus the above: DMA-BUF, GPU, driver pages, non-reclaimable cache...
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

func buildSample(t int64, mi map[string]int64, mmStat string) Sample {
	s := Sample{T: t, MemTotal: mi["MemTotal"], MemAvailable: mi["MemAvailable"]}
	s.Used = s.MemTotal - s.MemAvailable
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

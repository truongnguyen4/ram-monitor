package collector

import (
	"bufio"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Proc is one process from `dumpsys meminfo`. Sizes in kB.
type Proc struct {
	Name  string `json:"name"`
	PID   int    `json:"pid"`
	Group string `json:"group"`
	PSS   int64  `json:"pss"`
	Swap  int64  `json:"swap"`
}

// ProcSnapshot is the result of one `dumpsys meminfo` poll.
type ProcSnapshot struct {
	T           int64  `json:"t"`
	AndroidUsed int64  `json:"androidUsed"` // "Used RAM" line (used PSS + kernel), kB
	Procs       []Proc `json:"procs"`
}

var (
	reProcLine  = regexp.MustCompile(`^\s+([\d,]+)K: (.+?) \(pid (\d+)`)
	reGroupLine = regexp.MustCompile(`^\s+([\d,]+)K: ([^(]+?)\s*(?:\(.*\))?$`)
	reUsedRAM   = regexp.MustCompile(`^\s*Used RAM:\s*([\d,]+)K`)
	reVmSwap    = regexp.MustCompile(`/proc/(\d+)/status:VmSwap:\s+(\d+)\s*kB`)
)

func parseKB(s string) int64 {
	v, _ := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
	return v
}

// parseDumpsysMeminfo extracts per-process PSS (with OOM group) and the "Used RAM" total
// from the human-readable output of `dumpsys meminfo`.
func parseDumpsysMeminfo(text string) (procs []Proc, androidUsed int64) {
	const (
		none = iota
		byOOM
		byProcess
	)
	var fallback []Proc
	section := none
	group := ""
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case strings.HasPrefix(line, "Total PSS by OOM adjustment:"):
			section, group = byOOM, ""
			continue
		case strings.HasPrefix(line, "Total PSS by process:"):
			section = byProcess
			continue
		case strings.HasPrefix(line, "Total ") || strings.TrimSpace(line) == "":
			section = none
		}
		if m := reUsedRAM.FindStringSubmatch(line); m != nil {
			androidUsed = parseKB(m[1])
			continue
		}
		switch section {
		case byOOM:
			if m := reProcLine.FindStringSubmatch(line); m != nil {
				pid, _ := strconv.Atoi(m[3])
				procs = append(procs, Proc{Name: m[2], PID: pid, Group: group, PSS: parseKB(m[1])})
			} else if m := reGroupLine.FindStringSubmatch(line); m != nil {
				group = strings.TrimSpace(m[2])
			}
		case byProcess:
			if m := reProcLine.FindStringSubmatch(line); m != nil {
				pid, _ := strconv.Atoi(m[3])
				fallback = append(fallback, Proc{Name: m[2], PID: pid, PSS: parseKB(m[1])})
			}
		}
	}
	if len(procs) == 0 {
		procs = fallback
	}
	sort.SliceStable(procs, func(i, j int) bool { return procs[i].PSS > procs[j].PSS })
	return procs, androidUsed
}

// parseVmSwap parses `grep -H VmSwap /proc/<pid>/status ...` output into pid -> kB.
func parseVmSwap(out string) map[int]int64 {
	res := map[int]int64{}
	for _, m := range reVmSwap.FindAllStringSubmatch(out, -1) {
		pid, _ := strconv.Atoi(m[1])
		v, _ := strconv.ParseInt(m[2], 10, 64)
		res[pid] = v
	}
	return res
}

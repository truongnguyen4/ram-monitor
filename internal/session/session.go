// Package session holds the data of one monitoring session and writes it to CSV.
package session

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"ram-monitor/internal/collector"
)

// Info describes the device and session.
type Info struct {
	ID         int64  `json:"id"`
	Serial     string `json:"serial"`
	Model      string `json:"model"`
	Android    string `json:"android"`
	PhysicalGB int    `json:"physicalGB"` // estimated chip size, 0 = unknown yet
	StartedAt  int64  `json:"startedAt"`  // unix ms
	CSVPath    string `json:"csvPath"`
}

// ProcRow is a process plus its PSS change since first seen in this session.
type ProcRow struct {
	collector.Proc
	Delta int64 `json:"delta"`
}

// Procs is the latest process table.
type Procs struct {
	T           int64     `json:"t"`
	AndroidUsed int64     `json:"androidUsed"`
	Rows        []ProcRow `json:"rows"`
}

// ProcHistory is the PSS of every process at every dumpsys poll of the session.
// Each (process name, group) pair is stored once in Keys; a point refers to it by index,
// which keeps the history small enough to send to the UI on (re)load.
type ProcHistory struct {
	Keys   [][2]string `json:"keys"`   // [name, group]
	Points []ProcPoint `json:"points"` // in time order
}

// ProcPoint is one dumpsys poll: K[i] is an index into ProcHistory.Keys, P[i] its PSS in kB.
type ProcPoint struct {
	T int64   `json:"t"`
	K []int32 `json:"k"`
	P []int64 `json:"p"`
}

// Memory caps per session. Beyond them the older half is thinned 2:1, so a session of any length
// stays small in RAM and fast to send to the UI. The CSV files always keep every sample.
const (
	MaxSamples = 7200 // /proc/meminfo samples (2 h at 1 s, then coarser)
	MaxPoints  = 1440 // dumpsys polls (4 h at 10 s, then coarser)
)

// csvFile is a gzip-compressed CSV file. Each Flush ends a gzip block, so the file can be read
// (zcat, pandas, the Export button) while it is still being written.
type csvFile struct {
	f  *os.File
	gz *gzip.Writer
	w  *bufio.Writer
}

func newCSV(f *os.File, header string) *csvFile {
	gz, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	c := &csvFile{f: f, gz: gz, w: bufio.NewWriterSize(gz, 64*1024)}
	fmt.Fprintln(c.w, header)
	c.flush()
	return c
}

func (c *csvFile) flush() {
	_ = c.w.Flush()
	_ = c.gz.Flush()
}

func (c *csvFile) close() {
	_ = c.w.Flush()
	_ = c.gz.Close()
	_ = c.f.Close()
}

// Session stores samples in memory and appends them to CSV files.
type Session struct {
	mu           sync.Mutex
	info         Info
	samples      []collector.Sample
	totalSamples int
	procs        *Procs
	histKeys     [][2]string
	histIdx      map[[2]string]int32
	histPoints   []ProcPoint
	firstPSS     map[string]int64
	status       collector.Status
	paused       bool
	sys          *csvFile
	proc         *csvFile
	procPath     string
	pendingSys   int // samples written since the last flush
	nProcs       int // dumpsys polls recorded
	lastAndroid  int64
}

// sysFlushEvery bounds what a hard kill can lose (Close flushes everything).
const sysFlushEvery = 10

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// New creates a session and its CSV files under outDir.
func New(outDir string, info Info) (*Session, error) {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	start := time.UnixMilli(info.StartedAt)
	base := fmt.Sprintf("ram_%s_%s", unsafeChars.ReplaceAllString(info.Serial, "_"), start.Format("20060102_150405"))
	// Never overwrite an earlier session (e.g. two restarts within one second).
	var sysPath, procPath string
	var sysF *os.File
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s_%d", base, i)
		}
		sysPath = filepath.Join(outDir, name+".csv.gz")
		procPath = filepath.Join(outDir, name+"_procs.csv.gz")
		if _, err := os.Stat(procPath); err == nil {
			continue
		}
		f, err := os.OpenFile(sysPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sysF = f
		break
	}
	procF, err := os.Create(procPath)
	if err != nil {
		sysF.Close()
		return nil, err
	}
	s := &Session{
		info:     info,
		firstPSS: map[string]int64{},
		histIdx:  map[[2]string]int32{},
		sys:      newCSV(sysF, "time,elapsed_s,used_kb,mem_total_kb,mem_available_kb,anon_kb,shmem_kb,kernel_kb,zram_kb,other_kb,zram_stored_kb,swap_used_kb,android_used_kb,effective_total_kb,offline_kb,balloon_kb,limit_kb"),
		proc:     newCSV(procF, "time,elapsed_s,process,pid,group,pss_kb,swap_kb"),
		procPath: procPath,
		status:   collector.Status{Message: "connecting…"},
	}
	s.info.CSVPath = sysPath
	return s, nil
}

// Close flushes and closes the CSV files. A session that recorded nothing leaves no files behind.
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sys == nil {
		return
	}
	s.sys.close()
	s.proc.close()
	s.sys, s.proc = nil, nil
	if s.totalSamples == 0 && s.nProcs == 0 {
		_ = os.Remove(s.info.CSVPath)
		_ = os.Remove(s.procPath)
	}
}

// Info returns the session info.
func (s *Session) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

// SetPaused pauses or resumes recording.
func (s *Session) SetPaused(p bool) {
	s.mu.Lock()
	s.paused = p
	s.mu.Unlock()
}

// SetStatus records the connection status.
func (s *Session) SetStatus(st collector.Status) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

// AddSample stores a sample. It returns false when paused (sample dropped).
func (s *Session) AddSample(x collector.Sample) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paused || s.sys == nil {
		return false
	}
	if s.info.PhysicalGB == 0 {
		s.info.PhysicalGB = estimatePhysicalGB(x.MemTotal)
	}
	s.samples = append(s.samples, x)
	if len(s.samples) > MaxSamples {
		s.samples = ThinSamples(s.samples)
	}
	s.totalSamples++
	t := time.UnixMilli(x.T)
	fmt.Fprintf(s.sys.w, "%s,%.1f,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d,%d\n",
		t.Format("2006-01-02T15:04:05.000"), float64(x.T-s.info.StartedAt)/1000,
		x.Used, x.MemTotal, x.MemAvailable, x.Anon, x.Shmem, x.Kernel, x.Zram, x.Other, x.ZramOrig, x.SwapUsed, s.lastAndroid,
		x.EffTotal, x.Offline, x.Balloon, x.Limit)
	if s.pendingSys++; s.pendingSys >= sysFlushEvery {
		s.sys.flush()
		s.pendingSys = 0
	}
	return true
}

// SetProcs stores a process snapshot and returns the table to broadcast.
// It returns ok=false when paused.
func (s *Session) SetProcs(snap collector.ProcSnapshot) (Procs, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paused || s.sys == nil {
		return Procs{}, false
	}
	s.nProcs++
	s.lastAndroid = snap.AndroidUsed
	rows := make([]ProcRow, 0, len(snap.Procs))
	point := ProcPoint{T: snap.T, K: make([]int32, 0, len(snap.Procs)), P: make([]int64, 0, len(snap.Procs))}
	t := time.UnixMilli(snap.T)
	for _, p := range snap.Procs {
		key := [2]string{p.Name, p.Group}
		ki, ok := s.histIdx[key]
		if !ok {
			ki = int32(len(s.histKeys))
			s.histIdx[key] = ki
			s.histKeys = append(s.histKeys, key)
		}
		point.K = append(point.K, ki)
		point.P = append(point.P, p.PSS)
		first, seen := s.firstPSS[p.Name]
		if !seen {
			first = p.PSS
			s.firstPSS[p.Name] = p.PSS
		}
		rows = append(rows, ProcRow{Proc: p, Delta: p.PSS - first})
		fmt.Fprintf(s.proc.w, "%s,%.1f,%q,%d,%q,%d,%d\n",
			t.Format("2006-01-02T15:04:05.000"), float64(snap.T-s.info.StartedAt)/1000,
			p.Name, p.PID, p.Group, p.PSS, p.Swap)
	}
	s.proc.flush()
	s.histPoints = append(s.histPoints, point)
	if len(s.histPoints) > MaxPoints {
		s.histPoints = ThinPoints(s.histPoints)
	}
	pr := Procs{T: snap.T, AndroidUsed: snap.AndroidUsed, Rows: rows}
	s.procs = &pr
	return pr, true
}

// State is everything the UI needs on (re)load.
type State struct {
	Info    Info               `json:"info"`
	Paused  bool               `json:"paused"`
	Status  collector.Status   `json:"status"`
	Samples []collector.Sample `json:"samples"`
	Procs   *Procs             `json:"procs"`
	History ProcHistory        `json:"procHistory"`
}

// Snapshot returns a copy of the full state.
func (s *Session) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	samples := make([]collector.Sample, len(s.samples))
	copy(samples, s.samples)
	// Keys and points are never modified once appended, so copying the slice headers is enough.
	hist := ProcHistory{
		Keys:   append([][2]string(nil), s.histKeys...),
		Points: append([]ProcPoint(nil), s.histPoints...),
	}
	return State{Info: s.info, Paused: s.paused, Status: s.status, Samples: samples, Procs: s.procs, History: hist}
}

// Summary is the overview of one monitored device: its latest values and recent Used values.
type Summary struct {
	Info        Info              `json:"info"`
	Paused      bool              `json:"paused"`
	Status      collector.Status  `json:"status"`
	Last        *collector.Sample `json:"last"`
	AndroidUsed int64             `json:"androidUsed"`
	Recent      []int64           `json:"recent"` // Used (kB) of the last n samples
}

// Summary returns the overview of the session with the last n Used values.
func (s *Session) Summary(n int) Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	sum := Summary{Info: s.info, Paused: s.paused, Status: s.status, AndroidUsed: s.lastAndroid, Recent: []int64{}}
	if len(s.samples) > 0 {
		last := s.samples[len(s.samples)-1]
		sum.Last = &last
	}
	from := max(0, len(s.samples)-n)
	for _, x := range s.samples[from:] {
		sum.Recent = append(sum.Recent, x.Used)
	}
	return sum
}

// CSVPath returns the system CSV path (gzip) after flushing it.
func (s *Session) CSVPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sys != nil {
		s.sys.flush()
		s.pendingSys = 0
	}
	return s.info.CSVPath
}

// ThinSamples halves the resolution of the older half of xs. The first sample is kept and each
// later pair keeps the sample with the higher Used, so peaks survive thinning.
// The web UI applies the same rule (app.js thinSamples) to stay in step with the server.
func ThinSamples(xs []collector.Sample) []collector.Sample {
	half := len(xs) / 2
	out := make([]collector.Sample, 0, len(xs)-half/2)
	out = append(out, xs[0])
	i := 1
	for ; i+1 < half; i += 2 {
		if xs[i+1].Used >= xs[i].Used {
			out = append(out, xs[i+1])
		} else {
			out = append(out, xs[i])
		}
	}
	return append(out, xs[i:]...)
}

// ThinPoints halves the resolution of the older half of the dumpsys history (first point kept,
// later point of each pair kept). The web UI mirrors it (app.js thinPoints).
func ThinPoints(ps []ProcPoint) []ProcPoint {
	half := len(ps) / 2
	out := make([]ProcPoint, 0, len(ps)-half/2)
	out = append(out, ps[0])
	i := 1
	for ; i+1 < half; i += 2 {
		out = append(out, ps[i+1])
	}
	return append(out, ps[i:]...)
}

// estimatePhysicalGB rounds MemTotal (kB) up to a common DRAM size.
func estimatePhysicalGB(memTotalKB int64) int {
	gb := float64(memTotalKB) / (1024 * 1024)
	for _, size := range []int{1, 2, 3, 4, 6, 8, 10, 12, 16, 18, 20, 24, 32, 48, 64} {
		if gb <= float64(size) {
			return size
		}
	}
	return int(gb + 0.999)
}

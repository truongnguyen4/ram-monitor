// Package collector reads memory statistics from an Android device over adb.
package collector

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ram-monitor/internal/adb"
)

// Status describes the connection state of the collector.
type Status struct {
	Connected bool   `json:"connected"`
	Message   string `json:"message"`
}

// Collector streams /proc/meminfo every Interval and polls dumpsys meminfo every ProcInterval.
type Collector struct {
	ADB          *adb.Client
	Serial       string
	Interval     time.Duration
	ProcInterval time.Duration

	OnSample func(Sample)
	OnProcs  func(ProcSnapshot)
	OnStatus func(Status)
}

// Run blocks until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	go c.procLoop(ctx)
	c.streamLoop(ctx)
}

func (c *Collector) streamScript() string {
	secs := strconv.FormatFloat(c.Interval.Seconds(), 'f', -1, 64)
	return "while true; do echo @@B; cat /proc/meminfo; " +
		"echo \"@@Z $(cat /sys/block/zram0/mm_stat 2>/dev/null)\"; echo @@E; sleep " + secs + "; done"
}

func (c *Collector) streamLoop(ctx context.Context) {
	for ctx.Err() == nil {
		err := c.streamOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		msg := "lost connection to the device, retrying…"
		if err != nil {
			msg = fmt.Sprintf("connection lost: %v — retrying…", err)
		}
		c.OnStatus(Status{Connected: false, Message: msg})
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *Collector) streamOnce(ctx context.Context) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out, cmd, err := c.ADB.Stream(sctx, c.Serial, c.streamScript())
	if err != nil {
		return err
	}
	defer cmd.Wait() //nolint:errcheck

	sc := bufio.NewScanner(out)
	var lines []string
	var mm string
	in := false
	connected := false
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "@@B":
			in, lines, mm = true, lines[:0], ""
		case strings.HasPrefix(line, "@@Z"):
			mm = strings.TrimSpace(strings.TrimPrefix(line, "@@Z"))
		case line == "@@E":
			if !in {
				continue
			}
			in = false
			mi := parseMeminfo(lines)
			if mi["MemTotal"] == 0 {
				continue
			}
			if !connected {
				connected = true
				c.OnStatus(Status{Connected: true, Message: "recording"})
			}
			c.OnSample(buildSample(time.Now().UnixMilli(), mi, mm))
		case in:
			lines = append(lines, line)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !connected {
		return fmt.Errorf("cannot read /proc/meminfo")
	}
	return nil
}

func (c *Collector) procLoop(ctx context.Context) {
	for {
		start := time.Now()
		if snap, err := c.pollProcs(ctx); err == nil {
			c.OnProcs(snap)
		}
		wait := c.ProcInterval - time.Since(start)
		if wait < time.Second {
			wait = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (c *Collector) pollProcs(ctx context.Context) (ProcSnapshot, error) {
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := c.ADB.Shell(pctx, c.Serial, "dumpsys meminfo")
	if err != nil {
		return ProcSnapshot{}, err
	}
	procs, used := parseDumpsysMeminfo(string(out))
	if len(procs) == 0 {
		return ProcSnapshot{}, fmt.Errorf("cannot read the process list from dumpsys meminfo")
	}
	// Keep every process: the UI filters by name/group and then shows the top rows,
	// and the per-process CSV gets complete group totals.
	var b strings.Builder
	b.WriteString("grep -sH VmSwap")
	for _, p := range procs {
		fmt.Fprintf(&b, " /proc/%d/status", p.PID)
	}
	b.WriteString("; true")
	if sw, err := c.ADB.Shell(pctx, c.Serial, b.String()); err == nil {
		swap := parseVmSwap(string(sw))
		for i := range procs {
			procs[i].Swap = swap[procs[i].PID]
		}
	}
	return ProcSnapshot{T: time.Now().UnixMilli(), AndroidUsed: used, Procs: procs}, nil
}

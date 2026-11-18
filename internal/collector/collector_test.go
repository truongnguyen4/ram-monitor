package collector

import (
	"strings"
	"testing"
)

const meminfoFixture = `MemTotal:        5630420 kB
MemFree:          515436 kB
MemAvailable:    3548172 kB
Cached:          3294852 kB
SwapTotal:       3145724 kB
SwapFree:        2261756 kB
AnonPages:        626716 kB
Shmem:            123592 kB
SUnreclaim:       304188 kB
KernelStack:       58992 kB
ShadowCallStack:   14784 kB
PageTables:       120896 kB
VmallocUsed:      152280 kB
Percpu:            10048 kB`

func TestBuildSample(t *testing.T) {
	mi := parseMeminfo(strings.Split(meminfoFixture, "\n"))
	s := buildSample(1000, mi, "904044544 238317487 250654720        0 375123968    20951    39499     5299", 0, "")

	if s.Used != 5630420-3548172 {
		t.Errorf("Used = %d", s.Used)
	}
	if want := int64(304188 + 58992 + 14784 + 120896 + 152280 + 10048); s.Kernel != want {
		t.Errorf("Kernel = %d, want %d", s.Kernel, want)
	}
	if s.Zram != 250654720/1024 || s.ZramOrig != 904044544/1024 {
		t.Errorf("zram = %d / %d", s.Zram, s.ZramOrig)
	}
	if s.SwapUsed != 3145724-2261756 {
		t.Errorf("SwapUsed = %d", s.SwapUsed)
	}
	if got := s.Anon + s.Shmem + s.Kernel + s.Zram + s.Other; got != s.Used {
		t.Errorf("layers sum %d != Used %d", got, s.Used)
	}
}

func TestBuildSampleNoZram(t *testing.T) {
	mi := parseMeminfo(strings.Split(meminfoFixture, "\n"))
	s := buildSample(0, mi, "", 0, "")
	if s.Zram != 0 || s.Other < 0 {
		t.Errorf("unexpected zram=%d other=%d", s.Zram, s.Other)
	}
}

const dumpsysFixture = `Applications Memory Usage (in Kilobytes):
Uptime: 104064335 Realtime: 392115963

Total RSS by OOM adjustment:
    982,176K: Native
         60,000K: surfaceflinger (pid 1630)

Total PSS by process:
    222,517K: system (pid 2309)
     65,683K: com.android.launcher3 (pid 2918 / activities)
     30,852K: surfaceflinger (pid 1630)

Total PSS by OOM adjustment:
    416,891K: Native
         30,852K: surfaceflinger (pid 1630)
         13,480K: zygote (pid 1406)                       (    1,024K in swap)
    219,769K: System
        222,517K: system (pid 2309)
    120,000K: Foreground
         65,683K: com.android.launcher3 (pid 2918 / activities)

Total PSS by category:
    267,238K: Native

Total RAM: 5,630,420K (status normal)
 Free RAM: 3,954,256K (  850,428K cached pss + 2,588,844K cached kernel +   514,984K free)
 Used RAM: 2,268,735K (1,450,667K used pss +   818,068K kernel)
 Lost RAM:   150,857K
`

func TestParseDumpsysMeminfo(t *testing.T) {
	procs, used := parseDumpsysMeminfo(dumpsysFixture)
	if used != 2268735 {
		t.Errorf("androidUsed = %d", used)
	}
	want := []Proc{
		{Name: "system", PID: 2309, Group: "System", PSS: 222517},
		{Name: "com.android.launcher3", PID: 2918, Group: "Foreground", PSS: 65683},
		{Name: "surfaceflinger", PID: 1630, Group: "Native", PSS: 30852},
		{Name: "zygote", PID: 1406, Group: "Native", PSS: 13480},
	}
	if len(procs) != len(want) {
		t.Fatalf("got %d procs: %+v", len(procs), procs)
	}
	for i := range want {
		if procs[i] != want[i] {
			t.Errorf("procs[%d] = %+v, want %+v", i, procs[i], want[i])
		}
	}
}

func TestParseDumpsysFallback(t *testing.T) {
	text := "Total PSS by process:\n    222,517K: system (pid 2309)\n     30,852K: surfaceflinger (pid 1630)\n\n"
	procs, _ := parseDumpsysMeminfo(text)
	if len(procs) != 2 || procs[0].Name != "system" || procs[0].Group != "" {
		t.Errorf("fallback = %+v", procs)
	}
}

func TestParseVmSwap(t *testing.T) {
	out := "/proc/2309/status:VmSwap:\t    6084 kB\r\n/proc/1630/status:VmSwap:\t       0 kB\n"
	m := parseVmSwap(out)
	if m[2309] != 6084 || m[1630] != 0 || len(m) != 2 {
		t.Errorf("parseVmSwap = %v", m)
	}
}

func TestBuildSampleLimited(t *testing.T) {
	mi := parseMeminfo(strings.Split(meminfoFixture, "\n"))
	// 1 GB offlined (still in MemTotal) + 512 MB balloon
	managed := int64(5630420 - 1024*1024)
	s := buildSample(0, mi, "", managed, "target=4096M hidden=1024 balloon=512")
	if want := int64(5630420 - 1024*1024 - 512*1024); s.EffTotal != want {
		t.Errorf("EffTotal = %d, want %d", s.EffTotal, want)
	}
	if s.Offline != 1024*1024 || s.Balloon != 512*1024 || s.Limit != 4096*1024 {
		t.Errorf("Offline/Balloon/Limit = %d/%d/%d", s.Offline, s.Balloon, s.Limit)
	}
	if s.Used != s.EffTotal-s.MemAvailable {
		t.Errorf("Used = %d, want EffTotal-MemAvailable", s.Used)
	}
	// no limit: zones match MemTotal, no state file
	s = buildSample(0, mi, "", 5630420, "")
	if s.EffTotal != s.MemTotal || s.Offline != 0 || s.Balloon != 0 {
		t.Errorf("unlimited: EffTotal=%d Offline=%d Balloon=%d", s.EffTotal, s.Offline, s.Balloon)
	}
}

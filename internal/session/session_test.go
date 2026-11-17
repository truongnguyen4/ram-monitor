package session

import (
	"path/filepath"
	"testing"
	"time"

	"ram-monitor/internal/collector"
)

// Two sessions started in the same second (e.g. "Restart" clicked twice)
// must not overwrite each other's CSV files.
func TestNewDoesNotOverwrite(t *testing.T) {
	dir := t.TempDir()
	info := Info{Serial: "ABC:123", StartedAt: time.Date(2026, 11, 15, 10, 0, 0, 0, time.Local).UnixMilli()}

	var paths []string
	for i := 0; i < 3; i++ {
		s, err := New(dir, info)
		if err != nil {
			t.Fatal(err)
		}
		paths = append(paths, filepath.Base(s.Info().CSVPath))
		s.Close()
	}
	want := []string{
		"ram_ABC_123_20261115_100000.csv",
		"ram_ABC_123_20261115_100000_2.csv",
		"ram_ABC_123_20261115_100000_3.csv",
	}
	for i := range want {
		if paths[i] != want[i] {
			t.Errorf("session %d: got %s, want %s", i, paths[i], want[i])
		}
	}
	procs, _ := filepath.Glob(filepath.Join(dir, "*_procs.csv"))
	if len(procs) != 3 {
		t.Errorf("got %d procs files, want 3: %v", len(procs), procs)
	}
}

// The process history keeps one key per (name, group) and one point per poll,
// so the UI can rebuild component series after a reload.
func TestProcHistory(t *testing.T) {
	s, err := New(t.TempDir(), Info{Serial: "X", StartedAt: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	poll := func(ts int64, procs ...collector.Proc) {
		if _, ok := s.SetProcs(collector.ProcSnapshot{T: ts, Procs: procs}); !ok {
			t.Fatal("SetProcs dropped the snapshot")
		}
	}
	poll(2000, collector.Proc{Name: "a", Group: "Persistent", PSS: 10}, collector.Proc{Name: "b", Group: "Cached", PSS: 5})
	poll(3000, collector.Proc{Name: "a", Group: "Persistent", PSS: 12}, collector.Proc{Name: "b", Group: "Foreground", PSS: 7})
	s.SetPaused(true)
	if _, ok := s.SetProcs(collector.ProcSnapshot{T: 4000}); ok {
		t.Fatal("snapshot kept while paused")
	}

	h := s.Snapshot().History
	wantKeys := [][2]string{{"a", "Persistent"}, {"b", "Cached"}, {"b", "Foreground"}}
	if len(h.Keys) != len(wantKeys) {
		t.Fatalf("keys = %v, want %v", h.Keys, wantKeys)
	}
	for i := range wantKeys {
		if h.Keys[i] != wantKeys[i] {
			t.Errorf("key %d = %v, want %v", i, h.Keys[i], wantKeys[i])
		}
	}
	if len(h.Points) != 2 {
		t.Fatalf("points = %d, want 2", len(h.Points))
	}
	p := h.Points[1]
	if p.T != 3000 || len(p.K) != 2 || p.K[0] != 0 || p.K[1] != 2 || p.P[0] != 12 || p.P[1] != 7 {
		t.Errorf("point 1 = %+v", p)
	}
}

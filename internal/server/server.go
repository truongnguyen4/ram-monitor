// Package server serves the web UI and the JSON/SSE API.
package server

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"ram-monitor/internal/adb"
	"ram-monitor/internal/collector"
	"ram-monitor/internal/session"
)

// Config holds the server options.
type Config struct {
	ADB          *adb.Client
	OutDir       string
	Interval     time.Duration
	ProcInterval time.Duration
	TopN         int // rows shown in the process table (applied by the UI after filtering)
	Threshold    float64
	StateFile    string // remembers the monitored serials across restarts ("" = do not remember)
}

// Server owns one live session per monitored device and the SSE subscribers.
type Server struct {
	cfg Config
	web fs.FS

	mu     sync.Mutex
	live   map[string]*live // by serial
	order  []string         // serials in the order monitoring started
	wanted []string         // serials the user asked to monitor; re-attached when they come back
	nextID int64
	subs   map[*subscriber]struct{}
}

// subscriber is one SSE client. If its buffer fills up (a stalled tab), it is disconnected rather
// than silently missing events; the browser reconnects and reloads the full state.
type subscriber struct {
	ch   chan []byte
	gone chan struct{}
	once sync.Once
}

type live struct {
	sess   *session.Session
	cancel context.CancelFunc
}

// New creates a server.
func New(cfg Config, web fs.FS) *Server {
	return &Server{cfg: cfg, web: web, live: map[string]*live{}, subs: map[*subscriber]struct{}{}}
}

// loadWanted reads the serials remembered by an earlier run.
func (s *Server) loadWanted() []string {
	if s.cfg.StateFile == "" {
		return nil
	}
	b, err := os.ReadFile(s.cfg.StateFile)
	if err != nil {
		return nil
	}
	var list []string
	_ = json.Unmarshal(b, &list)
	return list
}

// setWanted adds or removes serial from the remembered set and saves it. Call with s.mu held.
func (s *Server) setWanted(serial string, on bool) {
	i := slices.Index(s.wanted, serial)
	switch {
	case on && i < 0:
		s.wanted = append(s.wanted, serial)
	case !on && i >= 0:
		s.wanted = slices.Delete(s.wanted, i, i+1)
	default:
		return
	}
	if s.cfg.StateFile == "" {
		return
	}
	b, _ := json.Marshal(s.wanted)
	tmp := s.cfg.StateFile + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.cfg.StateFile), 0o755); err == nil && os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, s.cfg.StateFile)
	}
}

// Watch re-attaches wanted devices that are not monitored (unplugged at start, or rebooted
// long enough for the session start to fail) as soon as adb lists them as ready.
func (s *Server) Watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		var missing []string
		for _, serial := range s.wanted {
			if s.live[serial] == nil {
				missing = append(missing, serial)
			}
		}
		s.mu.Unlock()
		if len(missing) == 0 {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		devs, err := s.cfg.ADB.Devices(dctx)
		cancel()
		if err != nil {
			continue
		}
		for _, d := range devs {
			if d.State == "device" && slices.Contains(missing, d.Serial) {
				if err := s.startSession(d.Serial); err != nil {
					log.Printf("[%s] re-attach failed: %v", d.Serial, err)
				}
			}
		}
	}
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	files := http.FileServer(http.FS(s.web))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache") // the UI changes with each build: never run a stale app.js
		files.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/devices", s.handleDevices)
	mux.HandleFunc("POST /api/device", s.handleSelectDevice)
	mux.HandleFunc("DELETE /api/device", s.handleStopDevice)
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/pause", s.handlePause)
	mux.HandleFunc("POST /api/restart", s.handleRestart)
	mux.HandleFunc("GET /api/export.csv", s.handleExport)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	return mux
}

// AutoSelect starts monitoring the serials in the comma-separated list, every ready device
// for "all", or the only attached device when the list is empty.
func (s *Server) AutoSelect(ctx context.Context, serials string) error {
	var list []string
	for _, x := range strings.Split(serials, ",") {
		if x = strings.TrimSpace(x); x != "" {
			list = append(list, x)
		}
	}
	if len(list) == 0 {
		if saved := s.loadWanted(); len(saved) > 0 {
			// Restore the devices monitored by the last run; absent ones are re-attached by Watch.
			s.mu.Lock()
			for _, serial := range saved {
				s.setWanted(serial, true)
			}
			s.mu.Unlock()
			devs, err := s.cfg.ADB.Devices(ctx)
			if err != nil {
				return err
			}
			for _, d := range devs {
				if d.State == "device" && slices.Contains(saved, d.Serial) {
					list = append(list, d.Serial)
				}
			}
			log.Printf("Restoring %d remembered devices (%d connected now)", len(saved), len(list))
			if len(list) == 0 {
				return nil
			}
		}
	}
	all := len(list) == 1 && list[0] == "all"
	if all {
		list = nil
	}
	if len(list) == 0 {
		devs, err := s.cfg.ADB.Devices(ctx)
		if err != nil {
			return err
		}
		for _, d := range devs {
			if d.State == "device" {
				list = append(list, d.Serial)
			}
		}
		switch {
		case len(list) == 0:
			return errors.New("no device connected; connect one and add it in the dashboard")
		case len(list) == 1 || all:
		default:
			// Never poll units nobody named: dumpsys every few seconds disturbs measurements running on them.
			return fmt.Errorf("%d devices are ready; add them in the dashboard or use -serial A,B", len(list))
		}
	}
	var errs []error
	for _, serial := range list {
		if s.get(serial) != nil {
			continue // already monitored
		}
		if err := s.startSession(serial); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", serial, err))
		}
	}
	return errors.Join(errs...)
}

// Shutdown stops every collector and closes the session files.
func (s *Server) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.live {
		l.cancel()
		l.sess.Close()
	}
	s.live = map[string]*live{}
	s.order = nil // the remembered set (state file) is kept for the next run
}

// stopSession stops monitoring serial. The session's CSV files are kept.
func (s *Server) stopSession(serial string) bool {
	s.mu.Lock()
	l, ok := s.live[serial]
	if ok {
		l.cancel()
		l.sess.Close()
		delete(s.live, serial)
		s.setWanted(serial, false)
		for i, x := range s.order {
			if x == serial {
				s.order = append(s.order[:i:i], s.order[i+1:]...)
				break
			}
		}
	}
	s.mu.Unlock()
	if ok {
		log.Printf("[%s] stopped", serial)
		s.broadcast("stopped", serial, nil)
	}
	return ok
}

func (s *Server) startSession(serial string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	props, err := s.cfg.ADB.Shell(ctx, serial, "getprop ro.product.model; getprop ro.build.version.release")
	if err != nil {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(string(props)), "\r", ""), "\n")
	info := session.Info{Serial: serial, StartedAt: time.Now().UnixMilli()}
	if len(lines) > 0 {
		info.Model = strings.TrimSpace(lines[0])
	}
	if len(lines) > 1 {
		info.Android = strings.TrimSpace(lines[1])
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.live[serial]; old != nil { // restart: replace this device's session
		old.cancel()
		old.sess.Close()
	} else {
		s.order = append(s.order, serial)
	}
	s.setWanted(serial, true)
	delete(s.live, serial)
	s.nextID++
	info.ID = s.nextID
	sess, err := session.New(s.cfg.OutDir, info)
	if err != nil {
		return err
	}
	runCtx, runCancel := context.WithCancel(context.Background())
	s.live[serial] = &live{sess: sess, cancel: runCancel}

	c := &collector.Collector{
		ADB:          s.cfg.ADB,
		Serial:       serial,
		Interval:     s.cfg.Interval,
		ProcInterval: s.cfg.ProcInterval,
		OnSample: func(x collector.Sample) {
			if s.get(serial) == sess && sess.AddSample(x) {
				s.broadcast("sample", serial, x)
			}
		},
		OnProcs: func(p collector.ProcSnapshot) {
			if s.get(serial) != sess {
				return
			}
			if pr, ok := sess.SetProcs(p); ok {
				s.broadcast("procs", serial, pr)
			}
		},
		OnStatus: func(st collector.Status) {
			if s.get(serial) != sess {
				return
			}
			sess.SetStatus(st)
			s.broadcast("status", serial, st)
			log.Printf("[%s] %s", serial, st.Message)
		},
	}
	go c.Run(runCtx)
	log.Printf("New session: %s (%s, Android %s) → %s", serial, info.Model, info.Android, sess.Info().CSVPath)
	go s.broadcast("session", serial, sess.Info())
	return nil
}

// get returns the live session of serial, or nil.
func (s *Server) get(serial string) *session.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l := s.live[serial]; l != nil {
		return l.sess
	}
	return nil
}

// sessionOf returns the session named by the "serial" query parameter, or the first one.
func (s *Server) sessionOf(r *http.Request) *session.Session {
	serial := r.URL.Query().Get("serial")
	s.mu.Lock()
	defer s.mu.Unlock()
	if serial == "" && len(s.order) > 0 {
		serial = s.order[0]
	}
	if l := s.live[serial]; l != nil {
		return l.sess
	}
	return nil
}

func (s *Server) sessions() []*session.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*session.Session, 0, len(s.order))
	for _, serial := range s.order {
		out = append(out, s.live[serial].sess)
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"intervalMs":     s.cfg.Interval.Milliseconds(),
		"procIntervalMs": s.cfg.ProcInterval.Milliseconds(),
		"threshold":      s.cfg.Threshold,
		"topN":           s.cfg.TopN,
		"maxSamples":     session.MaxSamples,
		"maxPoints":      session.MaxPoints,
	})
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	devs, err := s.cfg.ADB.Devices(r.Context())
	if err != nil {
		httpError(w, http.StatusBadGateway, err)
		return
	}
	if devs == nil {
		devs = []adb.Device{}
	}
	writeJSON(w, devs)
}

func (s *Server) handleSelectDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Serial string `json:"serial"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Serial == "" {
		httpError(w, http.StatusBadRequest, errors.New("missing serial"))
		return
	}
	if req.Serial == "all" {
		if err := s.AutoSelect(r.Context(), "all"); err != nil {
			httpError(w, http.StatusBadGateway, err)
			return
		}
	} else if s.get(req.Serial) == nil {
		if err := s.startSession(req.Serial); err != nil {
			httpError(w, http.StatusBadGateway, err)
			return
		}
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleStopDevice(w http.ResponseWriter, r *http.Request) {
	serial := r.URL.Query().Get("serial")
	if !s.stopSession(serial) {
		s.mu.Lock()
		was := slices.Contains(s.wanted, serial)
		s.setWanted(serial, false)
		s.mu.Unlock()
		if !was {
			httpError(w, http.StatusNotFound, errors.New("device is not monitored"))
			return
		}
		s.broadcast("stopped", serial, nil)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// handleSessions lists every monitored device with its latest values (overview strip),
// plus the wanted serials (monitored or waiting for the device to come back).
func (s *Server) handleSessions(w http.ResponseWriter, _ *http.Request) {
	out := []session.Summary{}
	for _, sess := range s.sessions() {
		out = append(out, sess.Summary(300))
	}
	s.mu.Lock()
	wanted := append([]string{}, s.wanted...)
	s.mu.Unlock()
	writeJSON(w, map[string]any{"sessions": out, "wanted": wanted})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionOf(r)
	if sess == nil {
		writeJSON(w, map[string]any{"info": nil})
		return
	}
	writeJSON(w, sess.Snapshot())
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paused bool `json:"paused"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	sess := s.sessionOf(r)
	if sess == nil {
		httpError(w, http.StatusConflict, errors.New("no device selected"))
		return
	}
	sess.SetPaused(req.Paused)
	s.broadcast("paused", sess.Info().Serial, req.Paused)
	writeJSON(w, map[string]bool{"paused": req.Paused})
}

// handleRestart clears the current data by starting a fresh session on the same
// device. The previous session's CSV files are kept on disk.
func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionOf(r)
	if sess == nil {
		httpError(w, http.StatusConflict, errors.New("no device selected"))
		return
	}
	if err := s.startSession(sess.Info().Serial); err != nil {
		httpError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	sess := s.sessionOf(r)
	if sess == nil {
		httpError(w, http.StatusConflict, errors.New("no device selected"))
		return
	}
	p := sess.CSVPath()
	f, err := os.Open(p)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	name := strings.TrimSuffix(filepath.Base(p), ".gz")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	// The file is still being written: it ends after the last flushed block, without a gzip trailer.
	if _, err := io.Copy(w, gz); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		log.Printf("export %s: %v", name, err)
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	sub := &subscriber{ch: make(chan []byte, 512), gone: make(chan struct{})}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, sub)
		s.mu.Unlock()
	}()

	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.gone: // fell behind: drop the stream, the browser reconnects and resyncs
			return
		case msg := <-sub.ch:
			if _, err := w.Write(msg); err != nil {
				return
			}
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// broadcast sends an event about one device; the payload is {"serial": ..., "v": ...}.
func (s *Server) broadcast(event, serial string, v any) {
	data, err := json.Marshal(struct {
		Serial string `json:"serial"`
		V      any    `json:"v"`
	}{serial, v})
	if err != nil {
		return
	}
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data))
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub := range s.subs {
		select {
		case sub.ch <- msg:
		default:
			sub.once.Do(func() { close(sub.gone) })
		}
	}
}

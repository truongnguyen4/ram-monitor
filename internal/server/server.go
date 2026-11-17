// Package server serves the web UI and the JSON/SSE API.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
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
}

// Server owns the current session and the SSE subscribers.
type Server struct {
	cfg Config
	web fs.FS

	mu     sync.Mutex
	sess   *session.Session
	cancel context.CancelFunc
	nextID int64
	subs   map[chan []byte]struct{}
}

// New creates a server.
func New(cfg Config, web fs.FS) *Server {
	return &Server{cfg: cfg, web: web, subs: map[chan []byte]struct{}{}}
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServer(http.FS(s.web)))
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/devices", s.handleDevices)
	mux.HandleFunc("POST /api/device", s.handleSelectDevice)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("POST /api/pause", s.handlePause)
	mux.HandleFunc("POST /api/restart", s.handleRestart)
	mux.HandleFunc("GET /api/export.csv", s.handleExport)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	return mux
}

// AutoSelect starts a session on serial, or on the only attached device when serial is empty.
func (s *Server) AutoSelect(ctx context.Context, serial string) error {
	if serial == "" {
		devs, err := s.cfg.ADB.Devices(ctx)
		if err != nil {
			return err
		}
		var ready []adb.Device
		for _, d := range devs {
			if d.State == "device" {
				ready = append(ready, d)
			}
		}
		switch len(ready) {
		case 0:
			return errors.New("no device connected; connect one and select it in the dashboard")
		case 1:
		default:
			return fmt.Errorf("%d devices are ready; select one in the dashboard or use -serial", len(ready))
		}
		serial = ready[0].Serial
	}
	return s.startSession(serial)
}

// Shutdown stops the collector and closes the session files.
func (s *Server) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	if s.sess != nil {
		s.sess.Close()
	}
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
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.sess != nil {
		s.sess.Close()
		s.sess = nil
	}
	s.nextID++
	info.ID = s.nextID
	sess, err := session.New(s.cfg.OutDir, info)
	if err != nil {
		return err
	}
	s.sess = sess
	runCtx, runCancel := context.WithCancel(context.Background())
	s.cancel = runCancel

	c := &collector.Collector{
		ADB:          s.cfg.ADB,
		Serial:       serial,
		Interval:     s.cfg.Interval,
		ProcInterval: s.cfg.ProcInterval,
		OnSample: func(x collector.Sample) {
			if s.current() == sess && sess.AddSample(x) {
				s.broadcast("sample", x)
			}
		},
		OnProcs: func(p collector.ProcSnapshot) {
			if s.current() != sess {
				return
			}
			if pr, ok := sess.SetProcs(p); ok {
				s.broadcast("procs", pr)
			}
		},
		OnStatus: func(st collector.Status) {
			if s.current() != sess {
				return
			}
			sess.SetStatus(st)
			s.broadcast("status", st)
			log.Printf("[%s] %s", serial, st.Message)
		},
	}
	go c.Run(runCtx)
	log.Printf("New session: %s (%s, Android %s) → %s", serial, info.Model, info.Android, sess.Info().CSVPath)
	go s.broadcast("session", sess.Info())
	return nil
}

func (s *Server) current() *session.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess
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
	if err := s.startSession(req.Serial); err != nil {
		httpError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleState(w http.ResponseWriter, _ *http.Request) {
	sess := s.current()
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
	sess := s.current()
	if sess == nil {
		httpError(w, http.StatusConflict, errors.New("no device selected"))
		return
	}
	sess.SetPaused(req.Paused)
	s.broadcast("paused", req.Paused)
	writeJSON(w, map[string]bool{"paused": req.Paused})
}

// handleRestart clears the current data by starting a fresh session on the same
// device. The previous session's CSV files are kept on disk.
func (s *Server) handleRestart(w http.ResponseWriter, _ *http.Request) {
	sess := s.current()
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
	sess := s.current()
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
	st, _ := f.Stat()
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(p)))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	http.ServeContent(w, r, filepath.Base(p), st.ModTime(), f)
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

	ch := make(chan []byte, 64)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
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
		case msg := <-ch:
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

func (s *Server) broadcast(event string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	msg := []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, data))
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- msg:
		default: // slow client: drop; it will resync on reconnect
		}
	}
}

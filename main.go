// Command ram-monitor shows the RAM usage of one or more Android devices in real time.
//
// It reads /proc/meminfo over a persistent `adb shell` session, polls
// `dumpsys meminfo` for per-process PSS, records everything to CSV and serves
// a dashboard on http://localhost:<port>.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"ram-monitor/internal/adb"
	"ram-monitor/internal/server"
)

//go:embed web
var webFS embed.FS

func main() {
	var (
		port         = flag.Int("port", 8080, "dashboard HTTP port on 127.0.0.1; one instance serves every device")
		adbPath      = flag.String("adb", "", "adb path (default: search PATH / Android SDK)")
		serial       = flag.String("serial", "", "device serials to monitor, comma-separated or \"all\" (default: the only connected device; add more in the dashboard)")
		interval     = flag.Duration("interval", time.Second, "/proc/meminfo polling interval")
		procInterval = flag.Duration("proc-interval", 10*time.Second, "dumpsys meminfo polling interval (process table)")
		topN         = flag.Int("top", 0, "limit the process table to the N largest rows after filtering (0 = show all processes)")
		threshold    = flag.Float64("threshold", 80, "warning threshold, % of MemTotal")
		outDir       = flag.String("out", "ram-monitor-data", "output directory for CSV files")
		noBrowser    = flag.Bool("no-browser", false, "do not open the browser automatically")
		stateFile    = flag.String("state", "", "file that remembers the monitored devices across restarts (default: <out>/monitored.json; \"none\" to disable)")
	)
	flag.Parse()
	log.SetFlags(log.Ltime)

	if *interval < 200*time.Millisecond {
		log.Fatal("-interval must be >= 200ms")
	}

	path, err := adb.Find(*adbPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Using adb: %s", path)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	srv := server.New(server.Config{
		ADB:          &adb.Client{Path: path},
		OutDir:       *outDir,
		Interval:     *interval,
		ProcInterval: *procInterval,
		TopN:         *topN,
		Threshold:    *threshold,
		StateFile:    statePath(*stateFile, *outDir),
	}, sub)

	// Listen before recording, so a busy port does not leave an empty session behind.
	// Only one port is ever opened: if a ram-monitor already serves it, hand the devices to it and exit.
	url := fmt.Sprintf("http://localhost:%d", *port)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		if !isRAMMonitor(*port) {
			log.Fatalf("cannot listen on 127.0.0.1:%d: %v (another program uses it; pick another -port)", *port, err)
		}
		log.Printf("ram-monitor is already running at %s; adding the devices there", url)
		handOff(*port, *serial)
		if !*noBrowser {
			openBrowser(url)
		}
		return
	}

	if err := srv.AutoSelect(context.Background(), *serial); err != nil {
		log.Printf("Not recording: %v", err)
	}
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	go srv.Watch(watchCtx, 15*time.Second)
	httpSrv := &http.Server{Handler: srv.Handler()}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	log.Printf("Dashboard: %s  (Ctrl+C to stop)", url)
	if !*noBrowser {
		openBrowser(url)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	log.Print("Stopping…")
	srv.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
}

func statePath(flagValue, outDir string) string {
	switch flagValue {
	case "none":
		return ""
	case "":
		return filepath.Join(outDir, "monitored.json")
	}
	return flagValue
}

// isRAMMonitor reports whether a ram-monitor dashboard answers on 127.0.0.1:port.
func isRAMMonitor(port int) bool {
	c := http.Client{Timeout: 2 * time.Second}
	r, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/config", port))
	if err != nil {
		return false
	}
	defer r.Body.Close()
	var cfg map[string]any
	if json.NewDecoder(r.Body).Decode(&cfg) != nil {
		return false
	}
	_, ok := cfg["procIntervalMs"]
	return ok
}

// handOff asks the running instance to monitor the serials (comma-separated) too.
func handOff(port int, serials string) {
	c := http.Client{Timeout: 15 * time.Second}
	for _, serial := range strings.Split(serials, ",") {
		if serial = strings.TrimSpace(serial); serial == "" {
			continue
		}
		body, _ := json.Marshal(map[string]string{"serial": serial})
		r, err := c.Post(fmt.Sprintf("http://127.0.0.1:%d/api/device", port), "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("%s: %v", serial, err)
			continue
		}
		r.Body.Close()
		log.Printf("%s: %s", serial, r.Status)
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("Cannot open the browser, please open %s manually", url)
		return
	}
	go cmd.Wait() //nolint:errcheck
}

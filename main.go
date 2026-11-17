// Command ram-monitor shows the RAM usage of an Android device in real time.
//
// It reads /proc/meminfo over a persistent `adb shell` session, polls
// `dumpsys meminfo` for per-process PSS, records everything to CSV and serves
// a dashboard on http://localhost:<port>.
package main

import (
	"context"
	"embed"
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
	"runtime"
	"syscall"
	"time"

	"ram-monitor/internal/adb"
	"ram-monitor/internal/server"
)

//go:embed web
var webFS embed.FS

func main() {
	var (
		port         = flag.Int("port", 8080, "dashboard HTTP port (if not set and 8080 is busy, the next free port up to 8099 is used)")
		adbPath      = flag.String("adb", "", "adb path (default: search PATH / Android SDK)")
		serial       = flag.String("serial", "", "device serial (default: auto-select if only one device is connected)")
		interval     = flag.Duration("interval", time.Second, "/proc/meminfo polling interval")
		procInterval = flag.Duration("proc-interval", 10*time.Second, "dumpsys meminfo polling interval (process table)")
		topN         = flag.Int("top", 0, "limit the process table to the N largest rows after filtering (0 = show all processes)")
		threshold    = flag.Float64("threshold", 80, "warning threshold, % of MemTotal")
		outDir       = flag.String("out", "ram-monitor-data", "output directory for CSV files")
		noBrowser    = flag.Bool("no-browser", false, "do not open the browser automatically")
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
	}, sub)

	// Listen before recording, so a busy port does not leave an empty session behind.
	portSet := false
	flag.Visit(func(f *flag.Flag) { portSet = portSet || f.Name == "port" })
	ln, err := listen(*port, portSet)
	if err != nil {
		log.Fatal(err)
	}
	url := fmt.Sprintf("http://localhost:%d", ln.Addr().(*net.TCPAddr).Port)

	if err := srv.AutoSelect(context.Background(), *serial); err != nil {
		log.Printf("Not recording yet: %v", err)
	}
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

// listen opens the dashboard port on 127.0.0.1. When the port was not given explicitly and is busy
// (e.g. another ram-monitor already monitors another device), the next free port is used.
func listen(port int, explicit bool) (net.Listener, error) {
	last := port
	if !explicit {
		last = port + 19
	}
	var err error
	for p := port; p <= last; p++ {
		var ln net.Listener
		if ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p)); err == nil {
			if p != port {
				log.Printf("Port %d is busy, using %d", port, p)
			}
			return ln, nil
		}
	}
	if explicit {
		return nil, fmt.Errorf("cannot listen on port %d: %v (try another -port)", port, err)
	}
	return nil, fmt.Errorf("no free port in %d-%d: %v (use -port)", port, last, err)
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

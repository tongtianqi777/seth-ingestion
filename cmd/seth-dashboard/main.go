// Command seth-dashboard is a local control panel for seth-ingestion and
// seth-feed. It runs a small web server on this computer and opens it in the
// browser, where you can link WhatsApp by scanning the QR code, start and
// stop the two services, and choose where the data lives.
//
// It's a wrapper: it runs the repo's run.sh and feed.sh scripts with the
// chosen folders, and reads what they print. It doesn't do their work itself.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
)

func main() {
	var (
		addr       = flag.String("addr", env("SETH_DASHBOARD_ADDR", "127.0.0.1:8090"), "address for the dashboard; keep it on 127.0.0.1")
		configPath = flag.String("config", env("SETH_DASHBOARD_CONFIG", "dashboard.json"), "where to save the dashboard's settings")
		open       = flag.Bool("open", true, "open the dashboard in the browser")
	)
	flag.Parse()
	logger := log.New(os.Stderr, "seth-dashboard ", log.LstdFlags)
	if err := run(*addr, *configPath, *open, logger); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(addr, configPath string, openBrowser bool, logger *log.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	repo, err := os.Getwd()
	if err != nil {
		return err
	}
	for _, script := range []string{"run.sh", "feed.sh"} {
		if _, err := os.Stat(filepath.Join(repo, script)); err != nil {
			return fmt.Errorf("run seth-dashboard from the seth-ingestion folder (it needs %s); ./dashboard.sh does that", script)
		}
	}
	config, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("read settings: %w", err)
	}
	d := &dashboard{
		repo: repo, configPath: configPath, log: logger, config: config,
		ingestion: newService("ingestion", repo, "./run.sh"),
		feed:      newService("feed", repo, "./feed.sh"),
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w (is the dashboard already running?)", addr, err)
	}
	url := "http://" + ln.Addr().String()
	if host, port, err := net.SplitHostPort(ln.Addr().String()); err == nil && (host == "127.0.0.1" || host == "::1") {
		url = "http://127.0.0.1:" + port
	}
	srv := &http.Server{Handler: d.handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	logger.Printf("Dashboard at %s. Press Ctrl+C to stop it and both services.", url)
	if openBrowser {
		openURL(url)
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	logger.Printf("Stopping the services")
	var wg sync.WaitGroup
	for _, s := range []*service{d.ingestion, d.feed} {
		wg.Add(1)
		go func() { defer wg.Done(); s.stop(15 * time.Second) }()
	}
	wg.Wait()
	shutdown, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// openURL opens url in the default browser, if it can.
func openURL(url string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	if err := exec.Command(name, url).Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Open %s in your browser.\n", url)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

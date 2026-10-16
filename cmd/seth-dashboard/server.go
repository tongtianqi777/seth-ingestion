package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"rsc.io/qr"

	"github.com/tianqitong/seth-ingestion/internal/paths"
)

//go:embed static
var static embed.FS

// Config is what the dashboard lets you change. Both services use the same
// folders, so the feed always serves what the ingestion records.
type Config struct {
	DataDir  string `json:"data_dir"`  // holds messages.db and the WhatsApp session
	MediaDir string `json:"media_dir"` // where photos and other files are saved
	FeedAddr string `json:"feed_addr"` // where seth-feed listens
}

func defaultConfig() Config {
	return Config{DataDir: paths.DefaultDataDir, MediaDir: paths.DefaultMediaDir, FeedAddr: "127.0.0.1:8080"}
}

func (c Config) validate() error {
	for name, v := range map[string]string{"data folder": c.DataDir, "media folder": c.MediaDir} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("the %s can't be empty", name)
		}
		if strings.ContainsAny(v, "\x00\n\r") {
			return fmt.Errorf("the %s contains characters a path can't have", name)
		}
	}
	host, port, err := net.SplitHostPort(c.FeedAddr)
	if err != nil {
		return fmt.Errorf("the feed address must look like 127.0.0.1:8080")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("the feed address needs a port between 1 and 65535")
	}
	if host != "" && net.ParseIP(host) == nil && host != "localhost" {
		return fmt.Errorf("the feed address must use an IP address, such as 127.0.0.1 or 0.0.0.0")
	}
	return nil
}

// dbPath is the message database inside the data folder, as seth-ingestion
// names it.
func (c Config) dbPath() string { return filepath.Join(c.DataDir, "messages.db") }

func (c Config) ingestionArgs() []string {
	return []string{"-data-dir", c.DataDir, "-media-dir", c.MediaDir, "-status-lines"}
}

func (c Config) feedArgs() []string {
	return []string{"-db", c.dbPath(), "-media-dir", c.MediaDir, "-addr", c.FeedAddr}
}

// dashboard is the control panel's state: the settings and the two services.
type dashboard struct {
	repo       string
	configPath string
	log        *log.Logger

	mu        sync.Mutex
	config    Config
	ingestion *service
	feed      *service
	storage   storageCache
}

func loadConfig(path string) (Config, error) {
	c := defaultConfig()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, c.validate()
}

func (d *dashboard) saveConfig(c Config) error {
	data, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(d.configPath, append(data, '\n'), 0o644)
}

// abs shows a path as the services will see it: "~" is the home folder, and
// other relative paths are relative to the repo, where the scripts run.
func (d *dashboard) abs(p string) string {
	p = paths.ExpandHome(p)
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(d.repo, p)
}

func (d *dashboard) handler() http.Handler {
	mux := http.NewServeMux()
	assets, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServerFS(assets))
	mux.HandleFunc("GET /api/status", d.status)
	mux.HandleFunc("GET /api/qr.png", d.qrImage)
	mux.HandleFunc("GET /api/metrics", d.metrics)
	mux.HandleFunc("GET /api/config", d.getConfig)
	mux.HandleFunc("PUT /api/config", d.putConfig)
	mux.HandleFunc("POST /api/{service}/{action}", d.control)
	return d.guard(mux)
}

// guard only lets the dashboard be used from a page it served itself.
//
//   - The Host header must be this computer, so a web page can't reach the
//     dashboard by pointing its own domain name at 127.0.0.1 (DNS rebinding).
//   - Requests that change something need the X-Seth-Dashboard header. A page
//     on another site can't add it without the dashboard's permission, which it
//     never gives, so it can't press the buttons for you.
func (d *dashboard) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		switch host {
		case "127.0.0.1", "localhost", "::1", "[::1]":
		default:
			writeError(w, http.StatusForbidden, "the dashboard only answers requests to 127.0.0.1 or localhost")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Seth-Dashboard") != "1" {
			writeError(w, http.StatusForbidden, "missing the X-Seth-Dashboard header; use the dashboard page")
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

type statusResponse struct {
	Ingestion Status `json:"ingestion"`
	Feed      Status `json:"feed"`
	Config    Config `json:"config"`
	Paths     struct {
		DataDir  string `json:"data_dir"`
		Database string `json:"database"`
		MediaDir string `json:"media_dir"`
	} `json:"paths"`
	FeedURL        string `json:"feed_url"`
	DatabaseExists bool   `json:"database_exists"`
}

// GET /api/status — everything the page shows, including the recent output.
func (d *dashboard) status(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	c := d.config
	d.mu.Unlock()
	var resp statusResponse
	resp.Ingestion = d.ingestion.status(true)
	resp.Feed = d.feed.status(true)
	resp.Config = c
	resp.Paths.DataDir, resp.Paths.Database, resp.Paths.MediaDir = d.abs(c.DataDir), d.abs(c.dbPath()), d.abs(c.MediaDir)
	_, err := os.Stat(resp.Paths.Database)
	resp.DatabaseExists = err == nil
	resp.FeedURL = feedURL(c.FeedAddr)
	writeJSON(w, http.StatusOK, resp)
}

// feedURL is where to open the feed from this computer.
func feedURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// GET /api/qr.png — the WhatsApp QR code to scan, while there is one.
func (d *dashboard) qrImage(w http.ResponseWriter, r *http.Request) {
	code := d.ingestion.currentQR()
	if code == "" {
		writeError(w, http.StatusNotFound, "no QR code to scan right now")
		return
	}
	img, err := qr.Encode(code, qr.L)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "couldn't draw the QR code")
		return
	}
	img.Scale = 8
	w.Header().Set("Content-Type", "image/png")
	w.Write(img.PNG())
}

func (d *dashboard) getConfig(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	writeJSON(w, http.StatusOK, d.config)
}

// PUT /api/config — change the folders or the feed address. Only while both
// services are stopped, so they never disagree about where the data is.
func (d *dashboard) putConfig(w http.ResponseWriter, r *http.Request) {
	var c Config
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		writeError(w, http.StatusBadRequest, "the settings must be JSON with data_dir, media_dir and feed_addr")
		return
	}
	c.DataDir, c.MediaDir, c.FeedAddr = strings.TrimSpace(c.DataDir), strings.TrimSpace(c.MediaDir), strings.TrimSpace(c.FeedAddr)
	if err := c.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if d.ingestion.status(false).Running || d.feed.status(false).Running {
		writeError(w, http.StatusConflict, "stop both services before changing the settings, so they keep using the same folders")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.saveConfig(c); err != nil {
		writeError(w, http.StatusInternalServerError, "couldn't save the settings: "+err.Error())
		return
	}
	d.config = c
	d.log.Printf("Settings saved: data %s, media %s, feed %s", c.DataDir, c.MediaDir, c.FeedAddr)
	writeJSON(w, http.StatusOK, c)
}

// POST /api/{ingestion|feed}/{start|stop}
func (d *dashboard) control(w http.ResponseWriter, r *http.Request) {
	var svc *service
	switch r.PathValue("service") {
	case "ingestion":
		svc = d.ingestion
	case "feed":
		svc = d.feed
	default:
		writeError(w, http.StatusNotFound, "no such service; use ingestion or feed")
		return
	}
	d.mu.Lock()
	c := d.config
	d.mu.Unlock()
	switch r.PathValue("action") {
	case "start":
		args := c.ingestionArgs()
		if svc == d.feed {
			args = c.feedArgs()
		}
		if err := svc.start(args...); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, errRunning) {
				status = http.StatusConflict
			}
			writeError(w, status, svc.name+": "+err.Error())
			return
		}
		d.log.Printf("Started %s", svc.name)
	case "stop":
		svc.stop(15 * time.Second)
		d.log.Printf("Stopped %s", svc.name)
	default:
		writeError(w, http.StatusNotFound, "no such action; use start or stop")
		return
	}
	writeJSON(w, http.StatusOK, svc.status(false))
}

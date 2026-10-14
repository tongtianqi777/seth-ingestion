package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeScript writes a shell script standing in for run.sh or feed.sh. It
// prints its arguments and lines, then waits until it's stopped.
func fakeScript(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("#!/bin/sh\ntrap 'echo stopping; exit 0' TERM\necho \"args: $*\"\n")
	for _, l := range lines {
		b.WriteString("echo '" + l + "'\n")
	}
	b.WriteString("while true; do sleep 0.05; done\n")
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestServiceTracksIngestionState(t *testing.T) {
	dir := t.TempDir()
	script := fakeScript(t, dir, "run.sh",
		"\x1b[36mINFO\x1b[0m Building seth-ingestion...",
		"@seth qr 2@first,code",
		"█▀▀▀▀▀█ ▄▀▄ █▀▀▀▀▀█",
		"@seth qr 2@second,code")
	s := newService("ingestion", dir, script)
	if err := s.start("-data-dir", "data", "-status-lines"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.stop(time.Second) })
	if err := s.start(); err != errRunning {
		t.Errorf("second start = %v, want errRunning", err)
	}
	eventually(t, "the second QR code", func() bool { return s.currentQR() == "2@second,code" })
	st := s.status(true)
	if !st.Running || st.State != stateWaiting || !st.HasQR || st.QRVersion == "" {
		t.Errorf("status = %+v", st)
	}
	log := strings.Join(st.Log, "\n")
	if !strings.Contains(log, "args: -data-dir data -status-lines") || !strings.Contains(log, "INFO Building") {
		t.Errorf("log = %q", log)
	}
	if strings.Contains(log, "█") || strings.Contains(log, "2@first") || strings.Contains(log, "\x1b") {
		t.Errorf("log has QR art, a QR code or colour codes: %q", log)
	}

	for _, c := range []struct{ line, state, detail string }{
		{"@seth status linked", stateLinked, ""},
		{"@seth status connected", stateConnected, ""},
		{"@seth status recording 17", stateRecording, "17 groups"},
	} {
		s.handleLine(c.line)
		if st := s.status(false); st.State != c.state || st.HasQR || st.Detail != c.detail {
			t.Errorf("after %q: %+v", c.line, st)
		}
	}

	s.stop(2 * time.Second)
	st = s.status(true)
	if st.Running || st.State != stateStopped || st.ExitError != "" {
		t.Errorf("after stop: %+v", st)
	}
	if !strings.Contains(strings.Join(st.Log, "\n"), "stopping") {
		t.Error("the script didn't get SIGTERM")
	}
}

func TestServiceReportsWhyItStopped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "feed.sh")
	os.WriteFile(path, []byte("#!/bin/sh\necho 'seth-feed starting'\necho 'error: open data/messages.db: unable to open database file' >&2\nexit 1\n"), 0o755)
	s := newService("feed", dir, path)
	if err := s.start(); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the script to exit", func() bool { return !s.status(false).Running })
	if st := s.status(false); st.State != stateStopped || !strings.Contains(st.ExitError, "unable to open database") {
		t.Errorf("status = %+v", st)
	}
}

func TestQRTimeoutKeepsItsState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.sh")
	os.WriteFile(path, []byte("#!/bin/sh\necho '@seth qr 2@code'\necho '@seth status qr_timeout'\necho 'error: QR code timed out' >&2\nexit 1\n"), 0o755)
	s := newService("ingestion", dir, path)
	s.start()
	eventually(t, "the script to exit", func() bool { return !s.status(false).Running })
	if st := s.status(false); st.State != stateTimedOut || st.HasQR {
		t.Errorf("status = %+v", st)
	}
}

func newTestDashboard(t *testing.T) (*dashboard, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	d := &dashboard{
		repo: dir, configPath: filepath.Join(dir, "dashboard.json"), log: log.New(io.Discard, "", 0), config: defaultConfig(),
		ingestion: newService("ingestion", dir, fakeScript(t, dir, "run.sh", "@seth qr 2@scan,me")),
		feed:      newService("feed", dir, fakeScript(t, dir, "feed.sh", "seth-feed Listening on http://127.0.0.1:8080. Press Ctrl+C to stop.")),
	}
	srv := httptest.NewServer(d.handler())
	t.Cleanup(func() {
		srv.Close()
		d.ingestion.stop(time.Second)
		d.feed.stop(time.Second)
	})
	return d, srv
}

func call(t *testing.T, srv *httptest.Server, method, path, body string, dashboardHeader bool) (int, []byte, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if dashboardHeader {
		req.Header.Set("X-Seth-Dashboard", "1")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data, resp.Header
}

func TestDashboardStartsBothWithTheSameFolders(t *testing.T) {
	d, srv := newTestDashboard(t)
	if code, _, _ := call(t, srv, "PUT", "/api/config", `{"data_dir":"school-data","media_dir":"/tmp/seth media","feed_addr":"127.0.0.1:9090"}`, true); code != 200 {
		t.Fatalf("save settings = %d", code)
	}
	saved, _ := os.ReadFile(d.configPath)
	if !strings.Contains(string(saved), `"school-data"`) {
		t.Errorf("settings file = %s", saved)
	}
	for _, svc := range []string{"ingestion", "feed"} {
		if code, body, _ := call(t, srv, "POST", "/api/"+svc+"/start", "", true); code != 200 {
			t.Fatalf("start %s = %d %s", svc, code, body)
		}
	}
	eventually(t, "the feed to be serving", func() bool { return d.feed.status(false).State == stateServing })
	eventually(t, "the QR code", func() bool { return d.ingestion.currentQR() != "" })

	var st statusResponse
	_, body, _ := call(t, srv, "GET", "/api/status", "", false)
	json.Unmarshal(body, &st)
	ingLog, feedLog := strings.Join(st.Ingestion.Log, "\n"), strings.Join(st.Feed.Log, "\n")
	if !strings.Contains(ingLog, "args: -data-dir school-data -media-dir /tmp/seth media -status-lines") {
		t.Errorf("ingestion args: %q", ingLog)
	}
	if !strings.Contains(feedLog, "args: -db school-data/messages.db -media-dir /tmp/seth media -addr 127.0.0.1:9090") {
		t.Errorf("feed args: %q", feedLog)
	}
	if st.FeedURL != "http://127.0.0.1:9090" || st.Paths.Database != filepath.Join(d.repo, "school-data", "messages.db") || !st.Ingestion.HasQR {
		t.Errorf("status = %+v", st)
	}

	// The QR code is a real PNG while there is one.
	code, img, hdr := call(t, srv, "GET", "/api/qr.png", "", false)
	if code != 200 || hdr.Get("Content-Type") != "image/png" {
		t.Fatalf("qr.png = %d %s", code, hdr.Get("Content-Type"))
	}
	if _, err := png.Decode(bytes.NewReader(img)); err != nil {
		t.Errorf("qr.png isn't a PNG: %v", err)
	}

	// Settings can't change while a service runs, so both keep the same folders.
	if code, _, _ := call(t, srv, "PUT", "/api/config", `{"data_dir":"x","media_dir":"y","feed_addr":"127.0.0.1:1"}`, true); code != http.StatusConflict {
		t.Errorf("save while running = %d, want 409", code)
	}
	if code, _, _ := call(t, srv, "POST", "/api/feed/start", "", true); code != http.StatusConflict {
		t.Errorf("second start = %d, want 409", code)
	}

	for _, svc := range []string{"ingestion", "feed"} {
		if code, _, _ := call(t, srv, "POST", "/api/"+svc+"/stop", "", true); code != 200 {
			t.Errorf("stop %s = %d", svc, code)
		}
	}
	if d.ingestion.status(false).Running || d.feed.status(false).Running {
		t.Error("still running after stop")
	}
	if code, _, _ := call(t, srv, "GET", "/api/qr.png", "", false); code != 404 {
		t.Errorf("qr.png after stop = %d, want 404", code)
	}
}

func TestDashboardRefusesOtherSites(t *testing.T) {
	_, srv := newTestDashboard(t)
	// No X-Seth-Dashboard header: what a form or fetch from another site would send.
	if code, _, _ := call(t, srv, "POST", "/api/ingestion/start", "", false); code != http.StatusForbidden {
		t.Errorf("start without the header = %d, want 403", code)
	}
	// A request for another host name, as with DNS rebinding.
	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.Host = "evil.example:8090"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("other Host = %d, want 403", resp.StatusCode)
	}
	if code, _, hdr := call(t, srv, "GET", "/", "", false); code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") || hdr.Get("X-Frame-Options") != "DENY" {
		t.Errorf("page = %d %v", code, hdr)
	}
}

func TestConfigValidation(t *testing.T) {
	for _, c := range []Config{
		{DataDir: "", MediaDir: "m", FeedAddr: "127.0.0.1:8080"},
		{DataDir: "d", MediaDir: "m\nx", FeedAddr: "127.0.0.1:8080"},
		{DataDir: "d", MediaDir: "m", FeedAddr: "8080"},
		{DataDir: "d", MediaDir: "m", FeedAddr: "127.0.0.1:99999"},
		{DataDir: "d", MediaDir: "m", FeedAddr: "example.com:80"},
	} {
		if c.validate() == nil {
			t.Errorf("%+v accepted", c)
		}
	}
	if err := defaultConfig().validate(); err != nil {
		t.Errorf("defaults rejected: %v", err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "dashboard.json")
	if c, err := loadConfig(path); err != nil || c != defaultConfig() {
		t.Errorf("missing settings file: %+v %v", c, err)
	}
	os.WriteFile(path, []byte(`{"data_dir":"d2"}`), 0o644)
	if c, err := loadConfig(path); err != nil || c.DataDir != "d2" || c.FeedAddr != "127.0.0.1:8080" {
		t.Errorf("partial settings file: %+v %v", c, err)
	}
}

func TestFeedURL(t *testing.T) {
	for addr, want := range map[string]string{"127.0.0.1:8080": "http://127.0.0.1:8080", "0.0.0.0:9000": "http://127.0.0.1:9000", ":8080": "http://127.0.0.1:8080"} {
		if got := feedURL(addr); got != want {
			t.Errorf("feedURL(%q) = %q, want %q", addr, got, want)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

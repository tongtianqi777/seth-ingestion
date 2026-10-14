package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// service runs one of the repo's scripts (run.sh or feed.sh) as a child
// process, keeps the tail of its output, and tracks its state from what it
// prints. It never does the script's work itself.
type service struct {
	name   string
	script []string // the command, e.g. ["./run.sh"]; arguments are added at start
	dir    string   // where to run it (the repo)

	mu        sync.Mutex
	cmd       *exec.Cmd
	done      chan struct{}
	running   bool
	stopping  bool
	state     string // see the state constants
	detail    string // e.g. how many groups are being recorded
	qr        string // the QR code to show, while waiting for a scan
	startedAt time.Time
	exitedAt  time.Time
	exitErr   string
	lines     []string // the last maxLines lines of output
}

const (
	stateStopped   = "stopped"
	stateStarting  = "starting"
	stateWaiting   = "waiting_for_scan" // ingestion: a QR code is showing
	stateLinked    = "linked"           // ingestion: scanned, connecting
	stateConnected = "connected"        // ingestion: connected, finding the groups
	stateRecording = "recording"        // ingestion: recording the groups
	stateUnlinked  = "unlinked"         // ingestion: unlinked on the phone; a new QR code follows
	stateTimedOut  = "qr_timeout"       // ingestion: nobody scanned in time
	stateServing   = "serving"          // feed: listening

	maxLines = 300
)

// Status is what the dashboard shows about a service.
type Status struct {
	Name      string   `json:"name"`
	Running   bool     `json:"running"`
	State     string   `json:"state"`
	Detail    string   `json:"detail,omitempty"`
	HasQR     bool     `json:"has_qr"`
	QRVersion string   `json:"qr_version,omitempty"` // changes with each new QR code
	StartedAt string   `json:"started_at,omitempty"`
	ExitedAt  string   `json:"exited_at,omitempty"`
	ExitError string   `json:"exit_error,omitempty"`
	PID       int      `json:"pid,omitempty"`
	Command   string   `json:"command,omitempty"`
	Log       []string `json:"log,omitempty"`
}

var errRunning = errors.New("already running")

func newService(name, dir string, script ...string) *service {
	return &service{name: name, script: script, dir: dir, state: stateStopped}
}

// start runs the script with args. Its output is read line by line.
func (s *service) start(args ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return errRunning
	}
	argv := append(append([]string{}, s.script[1:]...), args...)
	cmd := exec.Command(s.script[0], argv...)
	cmd.Dir = s.dir
	// Its own process group, so stopping it also stops what the script runs.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out := &lineWriter{onLine: s.handleLine}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd, s.done = cmd, make(chan struct{})
	s.running, s.stopping = true, false
	s.state, s.detail, s.qr, s.exitErr = stateStarting, "", "", ""
	s.startedAt, s.exitedAt = time.Now(), time.Time{}
	s.lines = append(s.lines, fmt.Sprintf("--- started %s %s", s.script[0], strings.Join(argv, " ")))
	go s.wait(cmd, out, s.done)
	return nil
}

func (s *service) wait(cmd *exec.Cmd, out *lineWriter, done chan struct{}) {
	err := cmd.Wait()
	out.flush()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running, s.exitedAt, s.qr = false, time.Now(), ""
	switch {
	case s.stopping:
		s.exitErr = ""
	case err != nil:
		s.exitErr = err.Error()
		if last := s.lastError(); last != "" {
			s.exitErr = last
		}
	}
	if s.state != stateTimedOut || s.stopping {
		s.state = stateStopped
	}
	s.lines = append(s.lines, "--- stopped")
	s.trim()
	close(done)
}

// lastError finds the most recent line that looks like an error, to show why
// the service stopped.
func (s *service) lastError() string {
	for i := len(s.lines) - 1; i >= 0 && i >= len(s.lines)-20; i-- {
		if l := s.lines[i]; strings.Contains(strings.ToLower(l), "error") {
			return l
		}
	}
	return ""
}

// stop asks the service to finish (SIGTERM, as Ctrl+C would), and kills it if
// it hasn't stopped within grace.
func (s *service) stop(grace time.Duration) {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.stopping = true
	pid, done := s.cmd.Process.Pid, s.done
	s.mu.Unlock()

	syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(grace):
		syscall.Kill(-pid, syscall.SIGKILL)
		<-done
	}
}

// handleLine keeps a line of output and updates the state from it.
// seth-ingestion's -status-lines output starts with "@seth".
func (s *service) handleLine(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if words := strings.Fields(line); len(words) >= 2 && words[0] == "@seth" {
		switch {
		case words[1] == "qr" && len(words) == 3:
			s.state, s.qr = stateWaiting, words[2]
			return // the code itself doesn't belong in the log
		case words[1] == "status" && len(words) >= 3:
			s.qr = ""
			s.state = words[2]
			if words[2] == stateRecording && len(words) == 4 {
				s.detail = words[3] + " groups"
			}
		}
		return
	}
	if isQRArt(line) {
		return // the terminal's QR code drawing; the dashboard shows an image instead
	}
	if s.name == "feed" && s.state == stateStarting && strings.Contains(line, "Listening on") {
		s.state = stateServing
	}
	s.lines = append(s.lines, line)
	s.trim()
}

func (s *service) trim() {
	if len(s.lines) > maxLines {
		s.lines = append([]string{}, s.lines[len(s.lines)-maxLines:]...)
	}
}

// isQRArt reports whether a line is part of a QR code drawn with block
// characters, as qrterminal prints it.
func isQRArt(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	for _, r := range line {
		switch r {
		case '█', '▀', '▄', ' ':
		default:
			return false
		}
	}
	return true
}

func (s *service) status(withLog bool) Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{Name: s.name, Running: s.running, State: s.state, Detail: s.detail, HasQR: s.qr != "", ExitError: s.exitErr}
	if s.qr != "" {
		st.QRVersion = strconv.FormatUint(uint64(fnv32(s.qr)), 16)
	}
	if !s.startedAt.IsZero() {
		st.StartedAt = s.startedAt.Format(time.RFC3339)
	}
	if !s.exitedAt.IsZero() {
		st.ExitedAt = s.exitedAt.Format(time.RFC3339)
	}
	if s.running {
		st.PID = s.cmd.Process.Pid
		st.Command = strings.Join(s.cmd.Args, " ")
	}
	if withLog {
		st.Log = append([]string{}, s.lines...)
	}
	return st
}

func (s *service) currentQR() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.qr
}

func fnv32(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// lineWriter splits a process's output into lines.
type lineWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	onLine func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for {
		i := bytes.IndexByte(w.buf.Bytes(), '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf.Next(i+1)), "\r\n")
		w.onLine(stripANSI(line))
	}
	return len(p), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len() > 0 {
		w.onLine(stripANSI(strings.TrimRight(w.buf.String(), "\r\n")))
		w.buf.Reset()
	}
}

// stripANSI removes terminal colour codes, which whatsmeow's logger adds.
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b[") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

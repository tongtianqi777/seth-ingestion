package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tianqitong/seth-ingestion/db"
)

// writeDB builds a message database the way seth-ingestion writes it.
func writeDB(t *testing.T, dataDir string) {
	t.Helper()
	ctx := context.Background()
	os.MkdirAll(dataDir, 0o755)
	s, err := db.Open(ctx, filepath.Join(dataDir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, g := range []db.Group{
		{JID: "1@g.us", Name: "Preschool", MediaFolder: "preschool"},
		{JID: "2@g.us", Name: "Announcements", IsAnnouncement: true, MediaFolder: "announcements"},
		{JID: "3@g.us", Name: "Acorn", MediaFolder: "acorn"},
	} {
		if _, err := s.SaveGroup(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	save := func(chat, id string, sent time.Time, files ...db.MediaFile) {
		t.Helper()
		m := &db.Message{ChatJID: chat, MessageID: id, Sender: "Melissa", SenderJID: "1@lid", SentAt: sent, MessageType: "text", Body: "hi", Source: "live"}
		if _, err := s.Save(ctx, m, files); err != nil {
			t.Fatal(err)
		}
	}
	save("1@g.us", "OLD", now.Add(-72*time.Hour))
	save("1@g.us", "PHOTO", now.Add(-2*time.Hour), db.MediaFile{SourceMessageID: "PHOTO", MediaType: "image", RelativePath: "preschool/a.jpg", Size: 1500})
	save("1@g.us", "ALBUM", now.Add(-time.Hour),
		db.MediaFile{SourceMessageID: "A1", MediaType: "image", RelativePath: "preschool/b.jpg", Size: 500},
		db.MediaFile{SourceMessageID: "A2", Position: 1, MediaType: "image", Error: "media expired"})
	save("2@g.us", "NEWEST", now.Add(-time.Minute), db.MediaFile{SourceMessageID: "NEWEST", MediaType: "document", Error: "media expired"})
	// A row from the earlier single-contact version: not in a recorded group, so not counted.
	save("15555550101@s.whatsapp.net", "DM", now, db.MediaFile{SourceMessageID: "DM", MediaType: "image", RelativePath: "x.jpg", Size: 9})
}

func TestReadMetrics(t *testing.T) {
	dir := t.TempDir()
	writeDB(t, dir)
	m, err := readMetrics(context.Background(), filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !m.Available || m.Messages != 4 || m.MessagesLast24h != 3 || m.MediaFiles != 4 || m.MediaFailed != 2 || m.MediaBytes != 2000 {
		t.Errorf("totals = %+v", m)
	}
	if m.NewestSentAt == nil || m.LastIngestedAt == nil {
		t.Fatalf("newest = %v, last ingested = %v", m.NewestSentAt, m.LastIngestedAt)
	}
	if newest, _ := time.Parse(time.RFC3339, *m.NewestSentAt); time.Since(newest) > 2*time.Minute {
		t.Errorf("newest_sent_at = %s, want the NEWEST message, not the DM", *m.NewestSentAt)
	}
	want := []GroupMetrics{
		{GroupJID: "2@g.us", Name: "Announcements", IsAnnouncement: true, Messages: 1, MediaFiles: 1, MediaFailed: 1},
		{GroupJID: "3@g.us", Name: "Acorn"},
		{GroupJID: "1@g.us", Name: "Preschool", Messages: 3, MediaFiles: 3, MediaFailed: 1},
	}
	if len(m.Groups) != len(want) {
		t.Fatalf("groups = %+v", m.Groups)
	}
	for i, w := range want {
		g := m.Groups[i]
		if g.GroupJID != w.GroupJID || g.Name != w.Name || g.IsAnnouncement != w.IsAnnouncement || g.Messages != w.Messages || g.MediaFiles != w.MediaFiles || g.MediaFailed != w.MediaFailed {
			t.Errorf("group %d = %+v, want %+v", i, g, w)
		}
		if (g.LastMessageAt == nil) != (w.Messages == 0) {
			t.Errorf("group %s last_message_at = %v", g.Name, g.LastMessageAt)
		}
	}
}

func TestMetricsEndpoint(t *testing.T) {
	d, srv := newTestDashboard(t)
	dataDir := filepath.Join(d.repo, "data")
	d.config.DataDir = "data"

	// Before the ingestion has ever run: no database, no error.
	code, body, _ := call(t, srv, "GET", "/api/metrics", "", false)
	var m Metrics
	json.Unmarshal(body, &m)
	if code != 200 || m.Available || m.Database != filepath.Join(dataDir, "messages.db") {
		t.Fatalf("no database: %d %s", code, body)
	}

	writeDB(t, dataDir)
	code, body, _ = call(t, srv, "GET", "/api/metrics", "", false)
	m = Metrics{}
	json.Unmarshal(body, &m)
	if code != 200 || !m.Available || m.Messages != 4 || m.MediaFailed != 2 || len(m.Groups) != 3 {
		t.Errorf("metrics: %d %s", code, body)
	}

	// Something that isn't a message database is reported, not a crash.
	d.config.DataDir = "elsewhere"
	writeFile(t, filepath.Join(d.repo, "elsewhere", "messages.db"), "not a database")
	if code, body, _ := call(t, srv, "GET", "/api/metrics", "", false); code != 503 {
		t.Errorf("broken database: %d %s", code, body)
	}
}

// TestMetricsWhileWriting reads the metrics again and again while another
// connection keeps writing, as seth-ingestion does while the dashboard polls.
func TestMetricsWhileWriting(t *testing.T) {
	dir := t.TempDir()
	writeDB(t, dir)
	path := filepath.Join(dir, "messages.db")
	ctx := context.Background()
	s, err := db.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 200; i++ {
			m := &db.Message{ChatJID: "3@g.us", MessageID: "W" + string(rune('A'+i%26)) + time.Now().Format("150405.000000"), SenderJID: "1@lid",
				SentAt: time.Now(), MessageType: "text", Body: "x", Source: "live"}
			if _, err := s.Save(ctx, m, []db.MediaFile{{SourceMessageID: m.MessageID, MediaType: "image", RelativePath: "acorn/x.jpg", Size: 1}}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	last := 0
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal("writer:", err)
			}
			m, err := readMetrics(ctx, path)
			if err != nil || m.Messages != 204 || m.MediaFiles != 204 {
				t.Errorf("final metrics: %d messages, %d files, %v; want 204 each", m.Messages, m.MediaFiles, err)
			}
			return
		default:
		}
		m, err := readMetrics(ctx, path)
		if err != nil {
			t.Fatal("reader:", err)
		}
		// One snapshot per read: every message counted has its file counted.
		if m.Messages < last || m.MediaFiles != m.Messages {
			t.Fatalf("inconsistent read: %d messages, %d files (previously %d)", m.Messages, m.MediaFiles, last)
		}
		last = m.Messages
	}
}

func TestStorage(t *testing.T) {
	d, srv := newTestDashboard(t)
	d.config.DataDir, d.config.MediaDir = "data", "media"
	writeDB(t, filepath.Join(d.repo, "data"))
	writeFile(t, filepath.Join(d.repo, "media", "preschool", "a.jpg"), "12345")
	writeFile(t, filepath.Join(d.repo, "media", "acorn", "b.jpg"), "123")

	_, body, _ := call(t, srv, "GET", "/api/metrics", "", false)
	var m Metrics
	json.Unmarshal(body, &m)
	s := m.Storage
	if s.MediaBytes != 8 || s.MediaFiles != 2 || s.DataBytes <= 0 || s.UsedBytes != s.MediaBytes+s.DataBytes {
		t.Errorf("storage = %+v", s)
	}
	if s.DiskTotal == 0 || s.DiskFree == 0 || s.DiskFree > s.DiskTotal || s.Error != "" {
		t.Errorf("disk = %+v", s)
	}

	// A media folder that doesn't exist yet is empty, and the disk is still measured.
	missing := readStorage(filepath.Join(d.repo, "nope", "media"), filepath.Join(d.repo, "nope", "data"))
	if missing.UsedBytes != 0 || missing.DiskTotal == 0 {
		t.Errorf("missing folders = %+v", missing)
	}
}

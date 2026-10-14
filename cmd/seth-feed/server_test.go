package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tianqitong/seth-ingestion/db"
)

const (
	preschoolJID = "120363000000000001@g.us"
	announceJID  = "120363000000000002@g.us"
)

var t0 = time.Date(2026, 10, 14, 16, 0, 0, 0, time.UTC)

// fixture is a message database written with the db package, as
// seth-ingestion writes it, served by a seth-feed handler.
type fixture struct {
	t        *testing.T
	store    *db.Store
	dbPath   string
	mediaDir string
	srv      *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	f := &fixture{t: t, dbPath: filepath.Join(dir, "messages.db"), mediaDir: filepath.Join(dir, "media")}
	var err error
	if f.store, err = db.Open(ctx, f.dbPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	for _, g := range []db.Group{
		{JID: preschoolJID, Name: "Preschool P.E. Teachers", CommunityName: "Seth", MediaFolder: "preschool-pe-teachers"},
		{JID: announceJID, Name: "Announcements", CommunityName: "Seth", IsAnnouncement: true, MediaFolder: "announcements"},
	} {
		if _, err := f.store.SaveGroup(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	os.MkdirAll(filepath.Join(f.mediaDir, "preschool-pe-teachers"), 0o755)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("outside the media folder"), 0o644)

	f.save(msg(preschoolJID, "T1", "text", "Pickup is at 3 today", 1), nil)
	reply := msg(preschoolJID, "T2", "text", "Thanks!", 2)
	reply.QuotedID = "T1"
	f.save(reply, nil)
	edit := msg(preschoolJID, "E1", "edit", "Pickup is at 3:30 today", 3)
	edit.EditTargetID = "T1"
	f.save(edit, nil)
	album := msg(preschoolJID, "ALB", "album", "Field trip", 4)
	album.RawJSON = ""
	f.save(album, []db.MediaFile{f.file("ALB-1", 0, "photo one")})
	f.save(msg(announceJID, "D1", "document", "Code of Conduct", 5), []db.MediaFile{{SourceMessageID: "D1", MediaType: "document", Mime: "application/pdf", FileName: "Code of Conduct.pdf", Error: "media expired"}})
	f.save(msg(announceJID, "EVIL", "image", "", 6), []db.MediaFile{{SourceMessageID: "EVIL", MediaType: "image", Mime: "text/plain", RelativePath: "../secret.txt"}})

	reader, err := openReader(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.close() })
	s := &server{db: reader, dbPath: f.dbPath, mediaDir: f.mediaDir, log: log.New(io.Discard, "", 0)}
	f.srv = httptest.NewServer(s.handler())
	t.Cleanup(f.srv.Close)
	return f
}

func msg(chat, id, typ, body string, minute int) *db.Message {
	return &db.Message{ChatJID: chat, MessageID: id, Sender: "Melissa Chen", SenderJID: "15555550101@s.whatsapp.net", SenderPhone: "15555550101",
		IsGroup: true, SentAt: t0.Add(time.Duration(minute) * time.Minute), MessageType: typ, Body: body, Source: "live", RawJSON: `{"conversation":"x"}`}
}

// file writes a JPEG-ish file into the media folder and returns its row.
func (f *fixture) file(source string, pos int, caption string) db.MediaFile {
	rel := "preschool-pe-teachers/" + source + ".jpg"
	data := []byte("\xff\xd8\xff photo " + source)
	os.WriteFile(filepath.Join(f.mediaDir, filepath.FromSlash(rel)), data, 0o644)
	return db.MediaFile{SourceMessageID: source, Position: pos, MediaType: "image", Caption: caption, Mime: "image/jpeg", RelativePath: rel, Size: int64(len(data)), SHA256: "x"}
}

func (f *fixture) save(m *db.Message, files []db.MediaFile) {
	f.t.Helper()
	if _, err := f.store.Save(context.Background(), m, files); err != nil {
		f.t.Fatal(err)
	}
}

// get fetches path and decodes the JSON response into v, checking the status.
func (f *fixture) get(path string, wantStatus int, v any) {
	f.t.Helper()
	resp, err := http.Get(f.srv.URL + path)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		f.t.Fatalf("GET %s = %d, want %d: %s", path, resp.StatusCode, wantStatus, body)
	}
	if v != nil {
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			f.t.Fatalf("GET %s: Content-Type %q, want JSON", path, ct)
		}
		if err := json.Unmarshal(body, v); err != nil {
			f.t.Fatalf("GET %s: %v: %s", path, err, body)
		}
	}
}

type page struct {
	Messages []Message        `json:"messages"`
	Cursor   string           `json:"cursor"`
	HasMore  bool             `json:"has_more"`
	Next     map[string]int64 `json:"next"`
}

func ids(msgs []Message) string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.MessageID)
	}
	return strings.Join(out, ",")
}

func TestHealthAndGroups(t *testing.T) {
	f := newFixture(t)
	var health map[string]any
	f.get("/health", 200, &health)
	if health["status"] != "ok" || health["simulated"] != false || health["events_available"] != false || health["messages"] != float64(6) {
		t.Errorf("health = %v", health)
	}
	if health["cursor"] != "6.3" { // 6 messages; 3 files
		t.Errorf("health cursor = %v, want 6.3", health["cursor"])
	}

	var groups struct{ Groups []Group }
	f.get("/groups", 200, &groups)
	if len(groups.Groups) != 2 || groups.Groups[0].Name != "Announcements" || groups.Groups[1].MessageCount != 4 {
		t.Errorf("groups = %+v", groups.Groups)
	}
}

func TestListMessages(t *testing.T) {
	f := newFixture(t)
	var p page
	f.get("/messages?limit=2", 200, &p)
	if ids(p.Messages) != "EVIL,D1" || !p.HasMore || p.Next["before"] != 5 {
		t.Fatalf("first page = %s, has_more %v, next %v", ids(p.Messages), p.HasMore, p.Next)
	}
	f.get("/messages?limit=2&before=5", 200, &p) // edits are left out by default
	if ids(p.Messages) != "ALB,T2" {
		t.Errorf("second page = %s", ids(p.Messages))
	}
	f.get("/messages?order=asc&include_edits=true", 200, &p)
	if ids(p.Messages) != "T1,T2,E1,ALB,D1,EVIL" || p.HasMore || p.Cursor != "6.3" {
		t.Errorf("oldest first with edits = %s, cursor %s", ids(p.Messages), p.Cursor)
	}

	// The group can be named by its name, its folder or its JID.
	for _, g := range []string{"Preschool%20P.E.%20Teachers", "preschool-pe-teachers", url.PathEscape(preschoolJID)} {
		f.get("/groups/"+g+"/messages?order=asc", 200, &p)
		if ids(p.Messages) != "T1,T2,ALB" {
			t.Errorf("group %s = %s", g, ids(p.Messages))
		}
	}
	f.get("/messages?group=announcements&order=asc", 200, &p)
	if ids(p.Messages) != "D1,EVIL" {
		t.Errorf("?group=announcements = %s", ids(p.Messages))
	}
	f.get("/messages?from=2026-10-14T16:02:00Z&to=2026-10-14T16:05:00Z&sort=sent_at&order=asc", 200, &p)
	if ids(p.Messages) != "T2,ALB" {
		t.Errorf("time range = %s", ids(p.Messages))
	}

	var e map[string]string
	f.get("/groups/nowhere/messages", 404, &e)
	f.get("/messages?limit=abc", 400, &e)
	f.get("/messages?order=sideways", 400, &e)
	f.get("/messages?sort=sent_at&since=3", 400, &e)
	if !strings.Contains(e["error"], "sort=seq") {
		t.Errorf("error = %q", e["error"])
	}
}

func TestMessageViewMatchesTheGuide(t *testing.T) {
	f := newFixture(t)
	var m Message
	f.get("/messages/"+url.PathEscape(preschoolJID)+"/T1", 200, &m)
	if *m.Body != "Pickup is at 3 today" || m.CurrentBody == nil || *m.CurrentBody != "Pickup is at 3:30 today" || !m.Edited || m.RawJSON == nil {
		t.Errorf("T1: body %v, current_body %v, edited %v, raw %v", m.Body, m.CurrentBody, m.Edited, m.RawJSON)
	}
	f.get("/messages/"+url.PathEscape(preschoolJID)+"/T2", 200, &m)
	if m.Quoted == nil || m.Quoted.MessageID != "T1" || *m.Quoted.CurrentBody != "Pickup is at 3:30 today" {
		t.Errorf("T2 quoted = %+v", m.Quoted)
	}
	f.get("/messages/"+url.PathEscape(preschoolJID)+"/ALB", 200, &m)
	if m.MessageType != "album" || len(m.Files) != 1 || m.Files[0].URL == nil || *m.Files[0].URL != "/media/1" {
		t.Errorf("album = %+v", m)
	}
	f.get("/messages/"+url.PathEscape(announceJID)+"/D1", 200, &m)
	if len(m.Files) != 1 || m.Files[0].Error == nil || m.Files[0].URL != nil {
		t.Errorf("failed download = %+v", m.Files)
	}
	var e map[string]string
	f.get("/messages/"+url.PathEscape(preschoolJID)+"/NOPE", 404, &e)
}

// TestFollow walks the follow loop: read everything, then read only what's
// new, including a photo added later to an album already read.
func TestFollow(t *testing.T) {
	f := newFixture(t)
	var p page
	f.get("/messages/new?limit=4", 200, &p)
	if ids(p.Messages) != "T1,T2,E1,ALB" || !p.HasMore || p.Cursor != "4.1" {
		t.Fatalf("first poll = %s, has_more %v, cursor %s", ids(p.Messages), p.HasMore, p.Cursor)
	}
	f.get("/messages/new?since="+p.Cursor, 200, &p)
	if ids(p.Messages) != "D1,EVIL" || p.HasMore || p.Cursor != "6.3" {
		t.Fatalf("second poll = %s, cursor %s", ids(p.Messages), p.Cursor)
	}
	cursor := p.Cursor
	f.get("/messages/new?since="+cursor, 200, &p)
	if len(p.Messages) != 0 || p.Cursor != cursor {
		t.Fatalf("nothing new: got %s, cursor %s", ids(p.Messages), p.Cursor)
	}

	// The album's second photo arrives, and a new message.
	album := msg(preschoolJID, "ALB", "album", "", 4)
	album.RawJSON = ""
	f.save(album, []db.MediaFile{f.file("ALB-2", 1, "")})
	f.save(msg(announceJID, "A2", "text", "Picture day is Thursday", 7), nil)

	f.get("/messages/new?since="+cursor, 200, &p)
	if ids(p.Messages) != "ALB,A2" || !p.Messages[0].Updated || p.Messages[1].Updated || len(p.Messages[0].Files) != 2 {
		t.Fatalf("after new writes = %s (updated %v), album files %d", ids(p.Messages), p.Messages[0].Updated, len(p.Messages[0].Files))
	}
	if p.Cursor != "7.4" {
		t.Errorf("cursor = %s, want 7.4", p.Cursor)
	}
	f.get("/messages/new?since="+p.Cursor, 200, &p)
	if len(p.Messages) != 0 {
		t.Errorf("repeat poll = %s", ids(p.Messages))
	}

	// A bare seq works as a cursor too, and a group filter applies.
	f.get("/messages/new?since=5&group=Announcements", 200, &p)
	if ids(p.Messages) != "EVIL,A2" {
		t.Errorf("since=5 in Announcements = %s", ids(p.Messages))
	}
	var e map[string]string
	f.get("/messages/new?since=abc", 400, &e)
}

func TestSearch(t *testing.T) {
	f := newFixture(t)
	var res struct {
		Query    string
		Messages []Message
	}
	f.get("/search?q=pickup", 200, &res)
	if ids(res.Messages) != "E1,T1" {
		t.Errorf("search pickup = %s", ids(res.Messages))
	}
	f.get("/search?q=photo%20one", 200, &res) // a file's caption
	if ids(res.Messages) != "ALB" {
		t.Errorf("search caption = %s", ids(res.Messages))
	}
	f.get("/search?q=conduct&group=preschool-pe-teachers", 200, &res)
	if len(res.Messages) != 0 {
		t.Errorf("search scoped to a group = %s", ids(res.Messages))
	}
	f.get("/search?q=100%25", 200, &res) // % is matched literally
	if len(res.Messages) != 0 {
		t.Errorf("search 100%% = %s", ids(res.Messages))
	}
	var e map[string]string
	f.get("/search?q=a", 400, &e)
}

func TestMedia(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.srv.URL + "/media/1")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" || string(body) != "\xff\xd8\xff photo ALB-1" {
		t.Errorf("media 1: %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}

	var e map[string]string
	f.get("/media/2", 404, &e) // the failed download
	if !strings.Contains(e["error"], "download failed") {
		t.Errorf("failed download error = %q", e["error"])
	}
	resp, err = http.Get(f.srv.URL + "/media/3") // relative_path ../secret.txt
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 404 || strings.Contains(string(body), "outside the media folder") {
		t.Errorf("path traversal: %d %s", resp.StatusCode, body)
	}
	f.get("/media/999", 404, &e)
	f.get("/media/abc", 400, &e)

	resp, err = http.Post(f.srv.URL+"/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", resp.StatusCode)
	}
}

func TestSafePath(t *testing.T) {
	s := &server{mediaDir: t.TempDir()}
	for _, rel := range []string{"../x", "a/../../x", "/etc/passwd", `a\..\x`, ""} {
		if _, ok := s.safePath(rel); ok {
			t.Errorf("safePath(%q) allowed", rel)
		}
	}
	if _, ok := s.safePath("preschool/20261014T160000Z_ID.jpg"); !ok {
		t.Error("safePath refused an ordinary path")
	}
	os.Symlink("/etc", filepath.Join(s.mediaDir, "link"))
	if _, ok := s.safePath("link/passwd"); ok {
		t.Error("safePath followed a symlink out of the media folder")
	}
}

func TestEventsOnlyWhenSimulated(t *testing.T) {
	f := newFixture(t)
	var e map[string]string
	f.get("/events", 404, &e)

	conn, err := sql.Open("sqlite3", "file:"+f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, stmt := range []string{
		`CREATE TABLE simulation (created_at TEXT, seed INTEGER, note TEXT)`,
		`CREATE TABLE simulated_events (chat_jid TEXT, message_id TEXT, event TEXT, action TEXT, starts_at TEXT,
			ends_on TEXT, all_day INTEGER, previous_starts_at TEXT, deadline_on TEXT, PRIMARY KEY (chat_jid, message_id))`,
		`INSERT INTO simulated_events VALUES ('` + announceJID + `', 'D1', 'picture day', 'scheduled', '2026-10-22', NULL, 1, NULL, NULL)`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	var events struct{ Events []SimulatedEvent }
	f.get("/events", 200, &events)
	if len(events.Events) != 1 || events.Events[0].Event != "picture day" || events.Events[0].Group != "Announcements" || !events.Events[0].AllDay {
		t.Errorf("events = %+v", events.Events)
	}
	var health map[string]any
	f.get("/health", 200, &health)
	if health["simulated"] != true || health["events_available"] != true {
		t.Errorf("health = %v", health)
	}
}

func TestReaderCantWrite(t *testing.T) {
	f := newFixture(t)
	r, err := openReader(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	if _, err := r.db.Exec(`DELETE FROM messages`); err == nil {
		t.Error("the read-only connection could delete messages")
	}
	if _, err := openReader(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Error("opened a database that doesn't exist")
	}
}

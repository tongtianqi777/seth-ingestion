package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func msg(id, typ, body string, at time.Time) *Message {
	return &Message{
		ChatJID: "1@lid", MessageID: id, Sender: "Ziwei Hu", SenderJID: "1@lid",
		SentAt: at, MessageType: typ, Body: body, Source: "live",
	}
}

func TestSaveAlbumIsOneMessage(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	t0 := time.Date(2026, 10, 12, 5, 0, 0, 0, time.UTC)

	// Items can arrive before the album's own message; each is saved under the album's ID.
	for i, item := range []struct{ id, caption string }{{"ITEM1", ""}, {"ITEM2", "beach"}} {
		m := msg("ALBUM", "album", item.caption, t0.Add(time.Duration(i+1)*time.Second))
		att := MediaFile{SourceMessageID: item.id, Position: i, MediaType: "image", Caption: item.caption, RelativePath: item.id + ".jpg"}
		if _, err := s.Save(ctx, m, []MediaFile{att}); err != nil {
			t.Fatal(err)
		}
	}
	header := msg("ALBUM", "album", "", t0)
	header.RawJSON = `{"albumMessage":{}}`
	isNew, err := s.Save(ctx, header, nil)
	if err != nil {
		t.Fatal(err)
	}
	if isNew {
		t.Error("album message should update the existing row, not create one")
	}

	var count, attCount int
	var body, raw, sentAt string
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT media_file_count, body, raw_json, sent_at FROM messages WHERE message_id = 'ALBUM'`).
		Scan(&attCount, &body, &raw, &sentAt); err != nil {
		t.Fatal(err)
	}
	if count != 1 || attCount != 2 {
		t.Errorf("got %d messages with %d media files, want 1 with 2", count, attCount)
	}
	if body != "beach" || raw != header.RawJSON || sentAt != t0.Format(time.RFC3339) {
		t.Errorf("album row not completed: body=%q raw_json=%q sent_at=%s", body, raw, sentAt)
	}

	// A re-synced item is recognized and not stored twice.
	if ok, _ := s.MediaFileExists(ctx, "1@lid", "ITEM2"); !ok {
		t.Error("MediaFileExists(ITEM2) = false")
	}
	if _, err := s.Save(ctx, msg("ALBUM", "album", "", t0), []MediaFile{{SourceMessageID: "ITEM2", MediaType: "image"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message_media_files`).Scan(&count); err != nil || count != 2 {
		t.Errorf("media files after duplicate = %d, %v; want 2", count, err)
	}
}

func TestMigrateLegacyMedia(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "messages.db")

	// A database as written by the previous version, with media columns on messages.
	legacy, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE messages (
			chat_jid TEXT NOT NULL, message_id TEXT NOT NULL, sender TEXT NOT NULL DEFAULT '',
			sender_jid TEXT NOT NULL, sender_phone TEXT, sender_name TEXT, is_group INTEGER NOT NULL DEFAULT 0,
			sent_at TEXT NOT NULL, ingested_at TEXT NOT NULL, message_type TEXT NOT NULL, body TEXT,
			edit_target_id TEXT, quoted_id TEXT, media_path TEXT, image_relative_path TEXT, media_mime TEXT,
			media_size INTEGER, media_sha256 TEXT, media_error TEXT, source TEXT NOT NULL, raw_json TEXT,
			PRIMARY KEY (chat_jid, message_id))`,
		`INSERT INTO messages VALUES ('c','TXT','Ziwei Hu','c',NULL,NULL,0,'t','t','text','hi',NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,'live',NULL)`,
		`INSERT INTO messages VALUES ('c','IMG1','Ziwei Hu','c',NULL,NULL,0,'t','t','image','cap',NULL,NULL,'/media/a.jpg','a.jpg','image/jpeg',10,'ab',NULL,'live',NULL)`,
		`INSERT INTO messages VALUES ('c','IMG2','Ziwei Hu','c',NULL,NULL,0,'t','t','image',NULL,NULL,NULL,'/media/sub/b.jpg',NULL,'image/jpeg',20,'cd',NULL,'live',NULL)`,
	} {
		if _, err := legacy.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	legacy.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for run, want := range []int{2, 0} { // second run is a no-op
		n, err := s.MigrateLegacyMedia(ctx, "/media")
		if err != nil || n != want {
			t.Fatalf("run %d: MigrateLegacyMedia = %d, %v; want %d", run, n, err, want)
		}
	}

	cols, _ := columns(ctx, s.db, "messages")
	for _, c := range legacyMediaColumns {
		if cols[c] {
			t.Errorf("column %s not dropped", c)
		}
	}
	rows, err := s.db.Query(`
		SELECT a.message_id, a.relative_path, a.caption, m.media_file_count
		FROM message_media_files a JOIN messages m USING (chat_jid, message_id) ORDER BY a.message_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, rel string
		var caption sql.NullString
		var n int
		if err := rows.Scan(&id, &rel, &caption, &n); err != nil {
			t.Fatal(err)
		}
		got = append(got, id+"|"+rel+"|"+caption.String+"|"+strconv.Itoa(n))
	}
	want := []string{"IMG1|a.jpg|cap|1", "IMG2|sub/b.jpg||1"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("media files = %v, want %v", got, want)
	}
}

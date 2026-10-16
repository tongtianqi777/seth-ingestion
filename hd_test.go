package main

import (
	"os"
	"path/filepath"
	"testing"

	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/tianqitong/seth-ingestion/db"
)

func hdRaw(parent string) string {
	return `{"imageMessage":{},"messageContextInfo":{"messageAssociation":{"associationType":"HD_IMAGE_DUAL_UPLOAD","parentMessageKey":{"ID":"` + parent + `"}}}}`
}

func TestMergeHDDuplicates(t *testing.T) {
	_, store, sqldb := newTestIngester(t)
	dir := t.TempDir()
	write := func(rel string) {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	save := func(m *db.Message, a db.MediaFile) {
		t.Helper()
		m.ChatJID, m.Sender, m.SenderJID, m.Source, m.SentAt = preschool.String(), "x", "x", "history", testSentAt
		if _, err := store.Save(testContext, m, []db.MediaFile{a}); err != nil {
			t.Fatal(err)
		}
	}
	// A single photo, an album item, and an HD message whose low-resolution parent isn't stored.
	save(&db.Message{MessageID: "SD1", MessageType: "image"}, db.MediaFile{SourceMessageID: "SD1", MediaType: "image", RelativePath: "g/sd1.jpg"})
	save(&db.Message{MessageID: "HD1", MessageType: "image", RawJSON: hdRaw("SD1")}, db.MediaFile{SourceMessageID: "HD1", MediaType: "image", RelativePath: "g/hd1.jpg", Size: 9})
	save(&db.Message{MessageID: "ALBUM", MessageType: "album"}, db.MediaFile{SourceMessageID: "SD2", Position: 3, MediaType: "image", RelativePath: "g/sd2.jpg"})
	save(&db.Message{MessageID: "HD2", MessageType: "image", RawJSON: hdRaw("SD2")}, db.MediaFile{SourceMessageID: "HD2", MediaType: "image", RelativePath: "g/hd2.jpg"})
	save(&db.Message{MessageID: "HD3", MessageType: "image", RawJSON: hdRaw("GONE")}, db.MediaFile{SourceMessageID: "HD3", MediaType: "image", RelativePath: "g/hd3.jpg"})
	for _, f := range []string{"sd1", "hd1", "sd2", "hd2", "hd3"} {
		write("g/" + f + ".jpg")
	}

	n, err := mergeHDDuplicates(testContext, store, dir, waLog.Noop)
	if err != nil || n != 2 {
		t.Fatalf("mergeHDDuplicates = %d, %v; want 2", n, err)
	}
	rows, err := sqldb.Query(`SELECT message_id, source_message_id, position, relative_path FROM message_media_files ORDER BY message_id, source_message_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var m, s, p string
		var pos int
		if err := rows.Scan(&m, &s, &pos, &p); err != nil {
			t.Fatal(err)
		}
		got = append(got, m+"/"+s+"/"+string(rune('0'+pos))+"/"+p)
	}
	want := []string{"ALBUM/SD2/3/g/hd2.jpg", "HD3/HD3/0/g/hd3.jpg", "SD1/SD1/0/g/hd1.jpg"}
	if len(got) != len(want) {
		t.Fatalf("media files = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("media files = %v, want %v", got, want)
			break
		}
	}
	for f, exists := range map[string]bool{"sd1": false, "sd2": false, "hd1": true, "hd2": true, "hd3": true} {
		if _, err := os.Stat(filepath.Join(dir, "g", f+".jpg")); (err == nil) != exists {
			t.Errorf("%s.jpg exists = %v, want %v", f, err == nil, exists)
		}
	}
	var msgs int
	if err := sqldb.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&msgs); err != nil || msgs != 3 {
		t.Errorf("messages = %d, %v; want 3 (SD1, ALBUM, HD3)", msgs, err)
	}

	// Running it again changes nothing.
	if n, err := mergeHDDuplicates(testContext, store, dir, waLog.Noop); err != nil || n != 0 {
		t.Errorf("second mergeHDDuplicates = %d, %v; want 0", n, err)
	}

	// An HD message found before its low-resolution parent arrives is found by the parent's ID.
	if id, ok, err := store.HDChildOf(testContext, preschool.String(), "GONE"); err != nil || !ok || id != "HD3" {
		t.Errorf("HDChildOf(GONE) = %q, %v, %v; want HD3", id, ok, err)
	}
	if _, ok, _ := store.HDChildOf(testContext, preschool.String(), "SD1"); ok {
		t.Error("HDChildOf(SD1) found a merged HD message")
	}
}

package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tianqitong/seth-ingestion/db"
)

func testConfig(t *testing.T, seed int64) common {
	t.Helper()
	dir := t.TempDir()
	groups := filepath.Join(dir, "groups.txt")
	if err := os.WriteFile(groups, []byte("Announcements\nPreschool\nCleanup Duties/Campus Security\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return common{dbPath: filepath.Join(dir, "sim", "messages.db"), mediaDir: filepath.Join(dir, "media"), groupsFile: groups, community: "Seth", seed: seed}
}

func seeded(t *testing.T, c common, days, perDay int) *sql.DB {
	t.Helper()
	ctx := context.Background()
	sim, err := open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if err := sim.seed(ctx, days, perDay); err != nil {
		t.Fatal(err)
	}
	if err := sim.live(ctx, 0, 3); err != nil {
		t.Fatal(err)
	}
	sim.close()
	conn, err := sql.Open("sqlite3", "file:"+c.dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func count(t *testing.T, conn *sql.DB, query string) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestSimulatedDataIsConsistent(t *testing.T) {
	c := testConfig(t, 42)
	conn := seeded(t, c, 5, 40)

	if n := count(t, conn, `SELECT COUNT(*) FROM messages`); n < 200 {
		t.Errorf("messages = %d, want at least 200", n)
	}
	if n := count(t, conn, `SELECT COUNT(*) FROM messages WHERE source = 'live'`); n != 3 {
		t.Errorf("live messages = %d, want 3", n)
	}
	if n := count(t, conn, `SELECT COUNT(*) FROM chat_groups WHERE community_name = 'Seth'`); n != 3 {
		t.Errorf("groups = %d, want 3", n)
	}
	for name, query := range map[string]string{
		"album without all its files": `SELECT COUNT(*) FROM messages m WHERE message_type = 'album' AND (raw_json IS NULL OR media_file_count < 2 OR
			media_file_count <> (SELECT COUNT(*) FROM message_media_files f WHERE f.chat_jid = m.chat_jid AND f.message_id = m.message_id))`,
		"reply to a missing message": `SELECT COUNT(*) FROM messages m WHERE quoted_id IS NOT NULL AND NOT EXISTS
			(SELECT 1 FROM messages q WHERE q.chat_jid = m.chat_jid AND q.message_id = m.quoted_id)`,
		"edit by someone else": `SELECT COUNT(*) FROM messages e WHERE message_type = 'edit' AND NOT EXISTS
			(SELECT 1 FROM messages o WHERE o.chat_jid = e.chat_jid AND o.message_id = e.edit_target_id AND o.sender_jid = e.sender_jid)`,
		"message outside the groups": `SELECT COUNT(*) FROM messages m WHERE NOT EXISTS (SELECT 1 FROM chat_groups g WHERE g.group_jid = m.chat_jid)`,
		"announcement not by staff":  `SELECT COUNT(*) FROM messages m JOIN chat_groups g ON g.group_jid = m.chat_jid WHERE g.is_announcement = 1 AND m.sender NOT IN ('Melissa Chen', 'Xiuxia Wang')`,
	} {
		if n := count(t, conn, query); n != 0 {
			t.Errorf("%d rows with %s", n, name)
		}
	}
	for _, typ := range []string{"text", "image", "document", "voice", "edit", "other"} {
		if n := count(t, conn, `SELECT COUNT(*) FROM messages WHERE message_type = '`+typ+`'`); n == 0 {
			t.Errorf("no %s messages", typ)
		}
	}
	if n := count(t, conn, `SELECT COUNT(*) FROM message_media_files WHERE error IS NOT NULL AND relative_path IS NULL`); n == 0 {
		t.Error("no failed downloads")
	}

	rows, err := conn.Query(`SELECT f.relative_path, f.size, g.media_folder FROM message_media_files f
		JOIN chat_groups g ON g.group_jid = f.chat_jid WHERE f.relative_path IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var rel, folder string
		var size int64
		if err := rows.Scan(&rel, &size, &folder); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(c.mediaDir, filepath.FromSlash(rel)))
		if err != nil || info.Size() != size || !strings.HasPrefix(rel, folder+"/") {
			t.Errorf("file %s: %v, size %d vs %d, folder %s", rel, err, size, info.Size(), folder)
		}
	}
}

func TestSameSeedSameMessages(t *testing.T) {
	bodies := func(seed int64) []string {
		conn := seeded(t, testConfig(t, seed), 2, 20)
		rows, err := conn.Query(`SELECT message_id || ' ' || message_type || ' ' || COALESCE(body, '') FROM messages ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			rows.Scan(&s)
			out = append(out, s)
		}
		return out
	}
	a, b := bodies(7), bodies(7)
	if !reflect.DeepEqual(a, b) {
		t.Error("the same seed gave different messages")
	}
	if reflect.DeepEqual(a, bodies(8)) {
		t.Error("different seeds gave the same messages")
	}
}

func TestRefusesRealDatabase(t *testing.T) {
	ctx := context.Background()

	// Next to a WhatsApp session, as data/messages.db is.
	c := testConfig(t, 1)
	os.MkdirAll(filepath.Dir(c.dbPath), 0o755)
	os.WriteFile(filepath.Join(filepath.Dir(c.dbPath), "whatsmeow-session.db"), nil, 0o644)
	if _, err := open(ctx, c); err == nil || !strings.Contains(err.Error(), "WhatsApp session") {
		t.Errorf("next to a session: err = %v", err)
	}

	// A database with messages that seth-sim didn't make.
	c = testConfig(t, 1)
	os.MkdirAll(filepath.Dir(c.dbPath), 0o755)
	store, err := db.Open(ctx, c.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, &db.Message{ChatJID: "1@g.us", MessageID: "REAL", SenderJID: "1@lid", MessageType: "text", Source: "live"}, nil); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if _, err := open(ctx, c); err == nil || !strings.Contains(err.Error(), "wasn't made by seth-sim") {
		t.Errorf("unmarked database: err = %v", err)
	}

	// A simulated database can be added to again.
	c = testConfig(t, 1)
	seeded(t, c, 1, 5)
	sim, err := open(ctx, c)
	if err != nil {
		t.Errorf("reopening a simulated database: %v", err)
	} else {
		sim.close()
	}
}

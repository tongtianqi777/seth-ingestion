package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/tianqitong/seth-ingestion/db"
)

var (
	ownJID      = types.NewJID("15550000001", types.DefaultUserServer)
	melissaJID  = types.NewJID("15550000002", types.DefaultUserServer)
	liwenLID    = types.NewJID("777000111", types.HiddenUserServer)
	liwenPN     = types.NewJID("15550000003", types.DefaultUserServer)
	preschool   = groupJID("120363000000000001")
	announce    = groupJID("120363000000000002")
	notListed   = groupJID("120363000000000009")
	testSentAt  = time.Date(2026, 10, 14, 17, 0, 0, 0, time.UTC)
	testContext = context.Background()
)

// newTestIngester returns an Ingester with a whatsmeow client that isn't
// connected, backed by temporary session and message databases. The returned
// *sql.DB is a separate connection to the message database, for checking it.
func newTestIngester(t *testing.T) (*Ingester, *db.Store, *sql.DB) {
	t.Helper()
	dir := t.TempDir()
	container, err := sqlstore.New(testContext, "sqlite3", "file:"+filepath.Join(dir, "session.db")+"?_foreign_keys=on", waLog.Noop)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { container.Close() })
	device := container.NewDevice()
	device.ID = &ownJID
	device.PushName = "Tianqi"
	// Placeholder signatures of the sizes the session database requires.
	device.Account = &waAdv.ADVSignedDeviceIdentity{
		Details:             []byte{0},
		AccountSignature:    make([]byte, 64),
		AccountSignatureKey: make([]byte, 32),
		DeviceSignature:     make([]byte, 64),
	}
	if err := device.Save(testContext); err != nil {
		t.Fatal(err)
	}

	// The implementation takes the first name, then the full name (the interface's parameter names are swapped).
	if err := device.Contacts.PutContactName(testContext, melissaJID, "Melissa", "Melissa Contact"); err != nil {
		t.Fatal(err)
	}
	if err := device.LIDs.PutLIDMapping(testContext, liwenLID, liwenPN); err != nil {
		t.Fatal(err)
	}

	store, err := db.Open(testContext, filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	check, err := sql.Open("sqlite3", "file:"+filepath.Join(dir, "messages.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { check.Close() })
	return NewIngester(whatsmeow.NewClient(device, waLog.Noop), store, filepath.Join(dir, "media"), waLog.Noop), store, check
}

func liveText(chat, sender types.JID, id, pushName, text string) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: chat.Server == types.GroupServer},
			ID:            id,
			PushName:      pushName,
			Timestamp:     testSentAt,
		},
		Message: &waE2E.Message{Conversation: proto.String(text)},
	}
}

func historyMsg(id string, fromMe bool, participant, pushName, text string) *waHistorySync.HistorySyncMsg {
	web := &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(fromMe)},
		MessageTimestamp: proto.Uint64(uint64(testSentAt.Unix())),
		PushName:         proto.String(pushName),
	}
	if participant != "" {
		web.Participant = proto.String(participant)
	}
	if text != "" {
		web.Message = &waE2E.Message{Conversation: proto.String(text)}
	}
	return &waHistorySync.HistorySyncMsg{Message: web}
}

func TestIngestMonitoredGroups(t *testing.T) {
	in, store, check := newTestIngester(t)
	go in.Run(testContext)

	// Events can arrive before the groups are known. They wait in the queue.
	in.HandleEvent(liveText(preschool, melissaJID, "LIVE1", "Mel", "Pickup is at 3 today"))
	in.HandleEvent(liveText(notListed, melissaJID, "OTHER1", "Mel", "not recorded: group isn't listed"))
	in.HandleEvent(liveText(melissaJID, melissaJID, "DM1", "Mel", "not recorded: direct chat"))
	in.HandleEvent(&events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{
		{ID: proto.String(preschool.String()), Messages: []*waHistorySync.HistorySyncMsg{
			historyMsg("HIST1", false, liwenLID.String(), "Liwen Lin", "I was going to do it today"),
			historyMsg("HIST2", true, "", "", "I will take your spot"),
			historyMsg("STUB1", false, liwenLID.String(), "", ""), // a notice such as "… joined"
		}},
		{ID: proto.String(notListed.String()), Messages: []*waHistorySync.HistorySyncMsg{
			historyMsg("OTHER2", false, melissaJID.String(), "Mel", "not recorded: group isn't listed"),
		}},
	}}})

	groups := map[types.JID]*db.Group{}
	for _, g := range []db.Group{
		{JID: preschool.String(), Name: "Preschool", MediaFolder: "preschool"},
		{JID: announce.String(), Name: "Announcements", IsAnnouncement: true, MediaFolder: "announcements"},
	} {
		saved, err := store.SaveGroup(testContext, g)
		if err != nil {
			t.Fatal(err)
		}
		jid, _ := types.ParseJID(saved.JID)
		groups[jid] = &saved
	}
	in.SetGroups(groups)

	// An album item whose file can't be downloaded: its media file row records the error.
	item := liveText(announce, melissaJID, "ITEM1", "Mel", "")
	item.Message = &waE2E.Message{
		ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), Caption: proto.String("Field trip")},
		MessageContextInfo: &waE2E.MessageContextInfo{MessageAssociation: &waE2E.MessageAssociation{
			AssociationType:  waE2E.MessageAssociation_MEDIA_ALBUM.Enum(),
			ParentMessageKey: &waCommon.MessageKey{ID: proto.String("ALBUM1")},
			MessageIndex:     proto.Int32(0),
		}},
	}
	in.HandleEvent(item)
	in.HandleEvent(liveText(notListed, melissaJID, "OTHER3", "Mel", "not recorded: group isn't listed"))
	in.HandleEvent(&events.GroupInfo{JID: preschool, Name: &types.GroupName{Name: "Preschool 2026"}})
	in.HandleEvent(&events.GroupInfo{JID: announce, Name: &types.GroupName{Name: "Seth"}})
	in.Stop()

	type row struct {
		chat, sender, senderPhone, msgType, body string
		isGroup, files                           int
	}
	rows, err := check.Query(`
		SELECT message_id, chat_jid, sender, COALESCE(sender_phone, ''), message_type, COALESCE(body, ''),
		       is_group, media_file_count
		FROM messages`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]row{}
	for rows.Next() {
		var id string
		var r row
		if err := rows.Scan(&id, &r.chat, &r.sender, &r.senderPhone, &r.msgType, &r.body, &r.isGroup, &r.files); err != nil {
			t.Fatal(err)
		}
		got[id] = r
	}
	rows.Close()
	want := map[string]row{
		"LIVE1":  {preschool.String(), "Melissa Contact", "15550000002", "text", "Pickup is at 3 today", 1, 0},
		"HIST1":  {preschool.String(), "Liwen Lin", "15550000003", "text", "I was going to do it today", 1, 0},
		"HIST2":  {preschool.String(), "Tianqi", "15550000001", "text", "I will take your spot", 1, 0}, // your own message
		"ALBUM1": {announce.String(), "Melissa Contact", "15550000002", "album", "Field trip", 1, 1},
	}
	if len(got) != len(want) {
		t.Errorf("stored messages %v, want only %v", keys(got), keys(want))
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("message %s = %+v, want %+v", id, got[id], w)
		}
	}

	var source string
	var relPath, dlErr sql.NullString
	if err := check.QueryRow(`SELECT source_message_id, relative_path, error FROM message_media_files WHERE message_id = 'ALBUM1'`).
		Scan(&source, &relPath, &dlErr); err != nil {
		t.Fatal(err)
	}
	if source != "ITEM1" || relPath.Valid || !dlErr.Valid {
		t.Errorf("album media file: source=%s relative_path=%v error=%v; want ITEM1, NULL path and an error", source, relPath, dlErr)
	}

	names := map[string]string{}
	rows, err = check.Query(`SELECT group_jid, name, media_folder FROM chat_groups`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var jid, name, folder string
		if err := rows.Scan(&jid, &name, &folder); err != nil {
			t.Fatal(err)
		}
		names[jid] = name + " | " + folder
	}
	rows.Close()
	if names[preschool.String()] != "Preschool 2026 | preschool" || names[announce.String()] != "Announcements | announcements" {
		t.Errorf("chat_groups = %v; want Preschool renamed with its folder kept, and Announcements unchanged", names)
	}
}

func TestStopBeforeGroupsAreKnown(t *testing.T) {
	in, _, check := newTestIngester(t)
	go in.Run(testContext)
	in.HandleEvent(liveText(preschool, melissaJID, "LIVE1", "Mel", "hello"))
	done := make(chan struct{})
	go func() { in.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop didn't return")
	}
	var n int
	if err := check.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil || n != 0 {
		t.Errorf("messages = %d, %v; want 0", n, err)
	}
}

func TestMediaRelPath(t *testing.T) {
	info := types.MessageInfo{ID: "3A37298F72373DFA1114", Timestamp: time.Date(2026, 10, 12, 5, 35, 8, 0, time.FixedZone("PDT", -7*3600))}
	for _, c := range []struct {
		folder string
		md     media
		want   string
	}{
		{"preschool", media{mime: "image/jpeg"}, "preschool/20261012T123508Z_3A37298F72373DFA1114.jpg"},
		{"spiritual-matters", media{mime: "application/pdf", fileName: "Code of Conduct.PDF"}, "spiritual-matters/20261012T123508Z_3A37298F72373DFA1114.pdf"},
		{"", media{mime: "audio/ogg; codecs=opus"}, "20261012T123508Z_3A37298F72373DFA1114.ogg"},
	} {
		if got := mediaRelPath(c.folder, info, &c.md); got != c.want {
			t.Errorf("mediaRelPath(%q, %+v) = %q, want %q", c.folder, c.md, got, c.want)
		}
	}
}

func keys[V any](m map[string]V) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

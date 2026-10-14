package main

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/tianqitong/seth-ingestion/db"
	"github.com/tianqitong/seth-ingestion/internal/grouplist"
)

type simulator struct {
	store    *db.Store
	dbPath   string
	read     *sql.DB // for looking up earlier messages to reply to, edit or react to
	mediaDir string
	rng      *rand.Rand
	groups   []*db.Group
}

func newSimulator(store *db.Store, dbPath, mediaDir string, seed int64) *simulator {
	return &simulator{store: store, dbPath: dbPath, mediaDir: mediaDir, rng: rand.New(rand.NewSource(seed))}
}

func (s *simulator) close() {
	if s.read != nil {
		s.read.Close()
	}
	s.store.Close()
}

func (s *simulator) groupNamed(name string) *db.Group {
	for _, g := range s.groups {
		if grouplist.Key(g.Name) == grouplist.Key(name) {
			return g
		}
	}
	return nil
}

// person is an invented community member. Phone numbers are in the 555-01xx
// range, which is reserved for fiction. Some people have only a LID, as many
// WhatsApp group members do, so clients see rows without sender_phone.
type person struct {
	name, phone, lid string
}

func (p person) jid() string {
	if p.phone != "" {
		return p.phone + "@s.whatsapp.net"
	}
	return p.lid + "@lid"
}

var people = []person{
	{"Melissa Chen", "15555550101", ""},
	{"David Park", "15555550102", ""},
	{"Xiuxia Wang", "15555550103", ""},
	{"Lingna Zhou", "15555550104", ""},
	{"Liwen Lin", "15555550105", ""},
	{"Shauna Brooks", "15555550106", ""},
	{"Selena Ortiz", "15555550107", ""},
	{"Qian Liu", "15555550108", ""},
	{"Nancy Yu", "15555550109", ""},
	{"Grace Kim", "15555550110", ""},
	{"Daniel Wu", "", "200000000000101"},
	{"Hannah Lee", "", "200000000000102"},
}

// staff post most announcements.
var staff = []int{0, 2}

// kind is one type of simulated message, with how often it occurs.
type kind struct {
	name   string
	weight int
}

var kinds = []*kind{
	{"text", 48}, {"reply", 12}, {"image", 10}, {"album", 5}, {"document", 5},
	{"voice", 5}, {"edit", 4}, {"reaction", 7}, {"failed", 4},
}

func kindByName(name string) *kind {
	for _, k := range kinds {
		if k.name == name {
			return k
		}
	}
	return nil
}

// event is one simulated message to write.
type event struct {
	group      *db.Group
	kind       *kind
	at         time.Time
	source     string // live | history
	from, text string // optional overrides (seth-sim send)
	target     string // optional message_id for reply, edit or reaction
}

func (s *simulator) seed(ctx context.Context, days, perDay int) error {
	end := time.Now()
	start := end.Add(-time.Duration(days) * 24 * time.Hour)
	n := days * perDay
	times := make([]time.Time, n)
	for i := range times {
		times[i] = s.schoolHour(start.Add(time.Duration(s.rng.Int63n(int64(end.Sub(start))))))
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	for _, at := range times {
		if ctx.Err() != nil {
			return nil
		}
		if at.After(end) {
			at = end
		}
		if err := s.emit(ctx, event{group: s.pickGroup(), kind: s.pickKind(), at: at, source: "history"}); err != nil {
			return err
		}
	}
	unit := "days"
	if days == 1 {
		unit = "day"
	}
	fmt.Printf("Added %d simulated messages over %d %s.\n", n, days, unit)
	return nil
}

func (s *simulator) live(ctx context.Context, interval time.Duration, count int) error {
	for i := 0; count == 0 || i < count; i++ {
		if i > 0 {
			wait := time.Duration(float64(interval) * (0.5 + s.rng.Float64()))
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(wait):
			}
		}
		if err := s.emit(ctx, event{group: s.pickGroup(), kind: s.pickKind(), at: time.Now(), source: "live"}); err != nil {
			return err
		}
	}
	return nil
}

// schoolHour moves t into 7am–9pm local time on the same day, when parents and
// teachers actually post.
func (s *simulator) schoolHour(t time.Time) time.Time {
	t = t.Local()
	day := time.Date(t.Year(), t.Month(), t.Day(), 7, 0, 0, 0, time.Local)
	return day.Add(time.Duration(s.rng.Int63n(int64(14 * time.Hour))))
}

func (s *simulator) pickGroup() *db.Group {
	// Announcements are rarer than chatter in the other groups.
	for {
		g := s.groups[s.rng.Intn(len(s.groups))]
		if !g.IsAnnouncement || s.rng.Intn(3) == 0 {
			return g
		}
	}
}

func (s *simulator) pickKind() *kind {
	total := 0
	for _, k := range kinds {
		total += k.weight
	}
	n := s.rng.Intn(total)
	for _, k := range kinds {
		if n < k.weight {
			return k
		}
		n -= k.weight
	}
	return kinds[0]
}

func (s *simulator) pickPerson(g *db.Group, name string) person {
	if name != "" {
		for _, p := range people {
			if strings.EqualFold(p.name, name) {
				return p
			}
		}
		return person{name: name, phone: fmt.Sprintf("1555555%04d", 200+s.rng.Intn(99))}
	}
	if g.IsAnnouncement {
		return people[staff[s.rng.Intn(len(staff))]]
	}
	return people[s.rng.Intn(len(people))]
}

// messageID makes an ID shaped like WhatsApp's: 20 hex characters starting with 3A.
func (s *simulator) messageID() string {
	return fmt.Sprintf("3A%016X%02X", s.rng.Uint64(), s.rng.Intn(256))
}

// simJID gives each group (and the community) a stable, invented group JID.
func simJID(name string) string {
	h := fnv.New64a()
	h.Write([]byte(grouplist.Key(name)))
	return fmt.Sprintf("1203639%011d@g.us", h.Sum64()%100000000000)
}

// emit writes one simulated message, and its files, exactly as seth-ingestion would.
func (s *simulator) emit(ctx context.Context, e event) error {
	g := e.group
	from := s.pickPerson(g, e.from)
	text := e.text
	if text == "" {
		text = s.line(g)
	}
	m := &db.Message{
		ChatJID: g.JID, MessageID: s.messageID(), Sender: from.name, SenderJID: from.jid(), SenderPhone: from.phone,
		SenderName: strings.Fields(from.name)[0], IsGroup: true, SentAt: e.at.UTC().Truncate(time.Second), Source: e.source,
	}
	var files []db.MediaFile
	var summary string

	switch e.kind.name {
	case "reply", "edit", "reaction":
		t, ok := s.target(ctx, g, e.target)
		if !ok {
			// Nothing to point at yet in this group: send a plain text instead.
			return s.emit(ctx, event{group: g, kind: kindByName("text"), at: e.at, source: e.source, from: e.from, text: e.text})
		}
		switch e.kind.name {
		case "reply":
			m.MessageType, m.Body, m.QuotedID = "text", text, t.id
			m.RawJSON = rawJSON(&waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
				Text: proto.String(text), ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String(t.id), Participant: proto.String(t.senderJID)}}})
			summary = fmt.Sprintf("%s (reply to %s)", text, t.id)
		case "edit":
			// Only the sender can edit a message, so the edit comes from them.
			m.Sender, m.SenderJID, m.SenderPhone, m.SenderName = t.sender, t.senderJID, t.senderPhone, strings.Fields(t.sender)[0]
			newText := t.body + " (updated: " + s.pick(editNotes) + ")"
			if e.text != "" {
				newText = e.text
			}
			m.MessageType, m.Body, m.EditTargetID = "edit", newText, t.id
			m.RawJSON = rawJSON(&waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
				Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String(t.id)},
				EditedMessage: &waE2E.Message{Conversation: proto.String(newText)}}})
			summary = fmt.Sprintf("%s (edit of %s)", newText, t.id)
		default:
			emoji := s.pick([]string{"👍", "❤️", "🙏", "😂", "🎉"})
			m.MessageType = "other"
			m.RawJSON = rawJSON(&waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
				Key: &waCommon.MessageKey{ID: proto.String(t.id)}, Text: proto.String(emoji)}})
			summary = fmt.Sprintf("reacted %s to %s", emoji, t.id)
		}

	case "image", "failed":
		caption := ""
		if e.text != "" || s.rng.Intn(2) == 0 {
			caption = s.captionOr(e.text)
		}
		m.MessageType, m.Body = "image", caption
		m.RawJSON = rawJSON(&waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Mimetype: proto.String("image/jpeg"), Caption: optional(caption), Width: proto.Uint32(640), Height: proto.Uint32(480)}})
		f := db.MediaFile{SourceMessageID: m.MessageID, MediaType: "image", Caption: caption, Mime: "image/jpeg"}
		if e.kind.name == "failed" {
			f.Error = "simulated: the file is no longer on WhatsApp's servers"
		} else if err := s.writeFile(g, m.MessageID, e.at, ".jpg", jpegImage(s.rng, g.Name), &f); err != nil {
			return err
		}
		files = append(files, f)
		summary = "photo " + caption
		if f.Error != "" {
			summary += " (download failed)"
		}

	case "album":
		return s.album(ctx, g, from, m, e)

	case "document":
		name := s.pick(documents)
		m.MessageType, m.Body = "document", strings.TrimSuffix(name, ".pdf")
		m.RawJSON = rawJSON(&waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
			Mimetype: proto.String("application/pdf"), FileName: proto.String(name), Title: proto.String(m.Body), PageCount: proto.Uint32(1)}})
		f := db.MediaFile{SourceMessageID: m.MessageID, MediaType: "document", FileName: name, Mime: "application/pdf"}
		if err := s.writeFile(g, m.MessageID, e.at, ".pdf", pdfDocument(m.Body, g.Name, from.name), &f); err != nil {
			return err
		}
		files = append(files, f)
		summary = "document " + name

	case "voice":
		secs := 3 + s.rng.Intn(40)
		m.MessageType = "voice"
		m.RawJSON = rawJSON(&waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			Mimetype: proto.String("audio/ogg; codecs=opus"), PTT: proto.Bool(true), Seconds: proto.Uint32(uint32(secs))}})
		f := db.MediaFile{SourceMessageID: m.MessageID, MediaType: "voice", Mime: "audio/ogg; codecs=opus"}
		if err := s.writeFile(g, m.MessageID, e.at, ".ogg", voicePlaceholder(secs), &f); err != nil {
			return err
		}
		files = append(files, f)
		summary = fmt.Sprintf("voice note, %ds", secs)

	default: // text
		m.MessageType, m.Body = "text", text
		m.RawJSON = rawJSON(&waE2E.Message{Conversation: proto.String(text)})
		summary = text
	}

	if _, err := s.store.Save(ctx, m, files); err != nil {
		return err
	}
	s.print(g, m, summary)
	return nil
}

// album writes an album the way it usually arrives: its photos first, each
// added to one album row, then the album message, which fills in raw_json.
func (s *simulator) album(ctx context.Context, g *db.Group, from person, m *db.Message, e event) error {
	n := 2 + s.rng.Intn(3)
	caption := s.captionOr(e.text)
	for i := 0; i < n; i++ {
		itemID := s.messageID()
		at := e.at.Add(time.Duration(i+1) * time.Second)
		item := *m
		item.MessageType, item.RawJSON, item.SentAt = "album", "", at.UTC().Truncate(time.Second)
		f := db.MediaFile{SourceMessageID: itemID, Position: i, MediaType: "image", Mime: "image/jpeg"}
		if i == 0 {
			item.Body, f.Caption = caption, caption
		}
		if err := s.writeFile(g, itemID, at, ".jpg", jpegImage(s.rng, g.Name), &f); err != nil {
			return err
		}
		if _, err := s.store.Save(ctx, &item, []db.MediaFile{f}); err != nil {
			return err
		}
	}
	m.MessageType = "album"
	m.RawJSON = rawJSON(&waE2E.Message{AlbumMessage: &waE2E.AlbumMessage{ExpectedImageCount: proto.Uint32(uint32(n))}})
	if _, err := s.store.Save(ctx, m, nil); err != nil {
		return err
	}
	s.print(g, m, fmt.Sprintf("album of %d photos %s", n, caption))
	return nil
}

// writeFile saves a simulated media file where seth-ingestion would put it,
// <group folder>/<UTC time>_<message id><ext> (see mediaRelPath in ingest.go),
// and fills in f's path, size and checksum.
func (s *simulator) writeFile(g *db.Group, id string, at time.Time, ext string, data []byte, f *db.MediaFile) error {
	rel := path.Join(g.MediaFolder, fmt.Sprintf("%s_%s%s", at.UTC().Format("20060102T150405Z"), id, ext))
	dst := filepath.Join(s.mediaDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	f.RelativePath, f.Size, f.SHA256 = rel, int64(len(data)), sha256Hex(data)
	return nil
}

type earlier struct{ id, sender, senderJID, senderPhone, body string }

// target finds a recent text message in g to reply to, edit or react to: the
// one with message_id id, or one of the last few.
func (s *simulator) target(ctx context.Context, g *db.Group, id string) (earlier, bool) {
	if s.read == nil {
		conn, err := sql.Open("sqlite3", "file:"+s.dbPath+"?mode=ro")
		if err != nil {
			return earlier{}, false
		}
		s.read = conn
	}
	q := `SELECT message_id, sender, sender_jid, COALESCE(sender_phone, ''), COALESCE(body, '') FROM messages
		WHERE chat_jid = ? AND message_type = 'text' ORDER BY rowid DESC LIMIT 5`
	args := []any{g.JID}
	if id != "" {
		q = `SELECT message_id, sender, sender_jid, COALESCE(sender_phone, ''), COALESCE(body, '') FROM messages
			WHERE chat_jid = ? AND message_id = ?`
		args = append(args, id)
	}
	rows, err := s.read.QueryContext(ctx, q, args...)
	if err != nil {
		return earlier{}, false
	}
	defer rows.Close()
	var found []earlier
	for rows.Next() {
		var t earlier
		if rows.Scan(&t.id, &t.sender, &t.senderJID, &t.senderPhone, &t.body) == nil {
			found = append(found, t)
		}
	}
	if len(found) == 0 {
		return earlier{}, false
	}
	return found[s.rng.Intn(len(found))], true
}

func (s *simulator) print(g *db.Group, m *db.Message, summary string) {
	fmt.Printf("%s  [%s] %s: %s\n", m.SentAt.Local().Format("Jan 02 15:04"), g.Name, m.Sender, strings.TrimSpace(summary))
}

func (s *simulator) pick(options []string) string { return options[s.rng.Intn(len(options))] }

func (s *simulator) captionOr(text string) string {
	if text != "" {
		return text
	}
	return s.pick(captions)
}

// line picks a message that suits the group, or a general one.
func (s *simulator) line(g *db.Group) string {
	if lines := groupLines[grouplist.Key(g.Name)]; len(lines) > 0 && s.rng.Intn(3) > 0 {
		return s.pick(lines)
	}
	return s.pick(generalLines)
}

func rawJSON(m *waE2E.Message) string {
	b, err := protojson.Marshal(m)
	if err != nil {
		return ""
	}
	return string(b)
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

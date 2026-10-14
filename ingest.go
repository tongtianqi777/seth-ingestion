package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/tianqitong/seth-ingestion/db"
)

// Ingester records the messages of the monitored groups.
//
// Events are queued by HandleEvent and processed by Run on a single worker.
// Which groups to monitor is only known once the client has connected, so the
// worker holds queued events until SetGroups is called.
type Ingester struct {
	client   *whatsmeow.Client
	store    *db.Store
	mediaDir string // images and other media are saved here, in a folder per group
	log      waLog.Logger

	// groups are the monitored groups by JID. SetGroups sets it once, before
	// closing ready; the map isn't changed after that. Only the worker reads or
	// changes the Group values.
	groups map[types.JID]*db.Group
	ready  chan struct{}

	mu      sync.RWMutex // guards stopped against concurrent enqueue during Stop
	stopped bool
	queue   chan job
	quit    chan struct{} // closed by Stop
	done    chan struct{}
}

// job is one queued event. Exactly one field is set.
type job struct {
	msg     *events.Message     // a live message
	history *events.HistorySync // chat history WhatsApp sends after linking
	group   *events.GroupInfo   // a group change, such as a new name
}

func NewIngester(client *whatsmeow.Client, store *db.Store, mediaDir string, log waLog.Logger) *Ingester {
	return &Ingester{
		client:   client,
		store:    store,
		mediaDir: mediaDir,
		log:      log,
		ready:    make(chan struct{}),
		queue:    make(chan job, 1024),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// SetGroups sets the groups to record and lets Run start on the queued events.
// Call it once.
func (in *Ingester) SetGroups(groups map[types.JID]*db.Group) {
	in.groups = groups
	close(in.ready)
}

// Run processes queued events on a single worker, so that media downloads
// never block whatsmeow's event loop. It returns after Stop drains the queue.
func (in *Ingester) Run(ctx context.Context) {
	defer close(in.done)
	select {
	case <-in.ready:
	case <-in.quit:
	}
	select {
	case <-in.ready:
	default:
		// Stopped before the groups were known, so nothing queued can be matched.
		for range in.queue {
		}
		return
	}
	for j := range in.queue {
		switch {
		case j.msg != nil:
			in.ingest(ctx, j.msg, "live")
		case j.history != nil:
			in.ingestHistory(ctx, j.history)
		case j.group != nil:
			in.updateGroup(ctx, j.group)
		}
	}
}

func (in *Ingester) Stop() {
	in.mu.Lock()
	in.stopped = true
	close(in.queue)
	close(in.quit)
	in.mu.Unlock()
	<-in.done
}

// HandleEvent is registered with whatsmeow's AddEventHandler. It runs on
// whatsmeow's event goroutine, so it only queues events.
func (in *Ingester) HandleEvent(rawEvt any) {
	switch evt := rawEvt.(type) {
	case *events.Message:
		if in.mayMonitor(evt.Info.Chat) {
			in.enqueue(job{msg: evt})
		}
	case *events.HistorySync:
		in.enqueue(job{history: evt})
	case *events.GroupInfo:
		if evt.Name != nil && in.mayMonitor(evt.JID) {
			in.enqueue(job{group: evt})
		}
	}
}

// mayMonitor reports whether chat is, or before SetGroups could be, a monitored
// group. Direct chats, status updates, broadcast lists and newsletters never are.
func (in *Ingester) mayMonitor(chat types.JID) bool {
	if chat.Server != types.GroupServer {
		return false
	}
	select {
	case <-in.ready:
		return in.groups[chat.ToNonAD()] != nil
	default:
		return true
	}
}

func (in *Ingester) enqueue(j job) {
	in.mu.RLock()
	defer in.mu.RUnlock()
	if !in.stopped {
		in.queue <- j
	}
}

// ingest stores evt if it was sent in a monitored group.
func (in *Ingester) ingest(ctx context.Context, evt *events.Message, source string) {
	g := in.groups[evt.Info.Chat.ToNonAD()]
	if g == nil {
		return
	}
	if err := in.process(ctx, evt, source, g); err != nil {
		in.log.Errorf("Failed to ingest message %s in %s: %v", evt.Info.ID, g.Name, err)
	}
}

func (in *Ingester) ingestHistory(ctx context.Context, evt *events.HistorySync) {
	for _, conv := range evt.Data.GetConversations() {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil || in.groups[chatJID.ToNonAD()] == nil {
			continue
		}
		for _, hm := range conv.GetMessages() {
			if hm.GetMessage().GetMessage() == nil {
				continue // a notice such as "… joined", which has no message content
			}
			msg, err := in.client.ParseWebMessage(chatJID, hm.GetMessage())
			if err != nil {
				in.log.Debugf("Skipping history message %s: %v", hm.GetMessage().GetKey().GetID(), err)
				continue
			}
			in.ingest(ctx, msg, "history")
		}
	}
}

// updateGroup records a monitored group's new name. Its media folder stays the
// same. The announcement group keeps its name, as WhatsApp keeps showing it as
// "Announcements".
func (in *Ingester) updateGroup(ctx context.Context, evt *events.GroupInfo) {
	g := in.groups[evt.JID.ToNonAD()]
	if g == nil || g.IsAnnouncement || evt.Name.Name == "" || evt.Name.Name == g.Name {
		return
	}
	in.log.Infof("Group %q was renamed to %q", g.Name, evt.Name.Name)
	if err := in.store.RenameGroup(ctx, g.JID, evt.Name.Name); err != nil {
		in.log.Errorf("Failed to record the new name of group %q: %v", g.Name, err)
		return
	}
	g.Name = evt.Name.Name
}

// senderPhone returns the sender's phone number. WhatsApp may address users by
// phone JID or by an opaque LID, so both the primary and alternate sender JIDs
// are checked, and LIDs are resolved through whatsmeow's LID mapping store.
func (in *Ingester) senderPhone(ctx context.Context, src types.MessageSource) string {
	for _, jid := range []types.JID{src.Sender, src.SenderAlt} {
		if phone := in.phoneFor(ctx, jid); phone != "" {
			return phone
		}
	}
	return ""
}

func (in *Ingester) phoneFor(ctx context.Context, jid types.JID) string {
	if jid.IsEmpty() {
		return ""
	}
	jid = jid.ToNonAD()
	switch jid.Server {
	case types.DefaultUserServer:
		return jid.User
	case types.HiddenUserServer:
		pn, err := in.client.Store.LIDs.GetPNForLID(ctx, jid)
		if err == nil && !pn.IsEmpty() {
			return pn.User
		}
	}
	return ""
}

// senderLabel names the sender the way WhatsApp does in a group: your saved
// contact name for them, otherwise the name they set for themselves, otherwise
// their phone number.
func (in *Ingester) senderLabel(ctx context.Context, info types.MessageInfo, phone string) string {
	var contact types.ContactInfo
	if phone != "" {
		contact, _ = in.client.Store.Contacts.GetContact(ctx, types.NewJID(phone, types.DefaultUserServer))
	}
	if !contact.Found {
		contact, _ = in.client.Store.Contacts.GetContact(ctx, info.Sender.ToNonAD())
	}
	names := []string{contact.FullName, contact.FirstName, info.PushName, contact.PushName, contact.BusinessName}
	if info.IsFromMe {
		names = append(names, in.client.Store.PushName)
	}
	for _, name := range names {
		if name != "" {
			return name
		}
	}
	if phone != "" {
		return "+" + phone
	}
	return info.Sender.User
}

func (in *Ingester) process(ctx context.Context, evt *events.Message, source string, g *db.Group) error {
	info := evt.Info
	msg := evt.Message
	// whatsmeow doesn't unwrap album items, which may arrive wrapped in an AssociatedChildMessage.
	if child := msg.GetAssociatedChildMessage().GetMessage(); child != nil {
		if child.MessageContextInfo == nil {
			child.MessageContextInfo = msg.MessageContextInfo
		}
		msg = child
	}

	phone := in.senderPhone(ctx, info.MessageSource)
	m := &db.Message{
		ChatJID:     g.JID,
		MessageID:   info.ID,
		Sender:      in.senderLabel(ctx, info, phone),
		SenderJID:   info.Sender.ToNonAD().String(),
		SenderPhone: phone,
		SenderName:  info.PushName,
		IsFromMe:    info.IsFromMe,
		IsGroup:     true,
		SentAt:      info.Timestamp,
		Source:      source,
	}
	if raw, err := protojson.Marshal(msg); err == nil {
		m.RawJSON = string(raw)
	}

	var atts []db.MediaFile
	md := mediaOf(msg)
	switch {
	case msg.GetProtocolMessage().GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT:
		pm := msg.GetProtocolMessage()
		m.MessageType = "edit"
		m.EditTargetID = pm.GetKey().GetID()
		m.Body = textOf(pm.GetEditedMessage())
	case msg.GetAlbumMessage() != nil:
		// The album's own message carries no files; its items arrive as separate messages.
		m.MessageType = "album"
	case md != nil:
		seen, err := in.store.MediaFileExists(ctx, m.ChatJID, info.ID)
		if err != nil || seen {
			return err
		}
		att := in.saveMedia(ctx, md, info, g.MediaFolder)
		if parentID, index, ok := albumParent(msg); ok {
			// Store the file under the album's message, so the album stays one messages row.
			// The album's own message fills in raw_json when it arrives.
			m.MessageID = parentID
			m.MessageType = "album"
			m.RawJSON = ""
			att.Position = index
		} else {
			m.MessageType = md.kind
			m.QuotedID = md.context.GetStanzaID()
		}
		m.Body = md.caption
		atts = append(atts, att)
	case msg.GetConversation() != "" || msg.GetExtendedTextMessage() != nil:
		m.MessageType = "text"
		m.Body = textOf(msg)
		m.QuotedID = msg.GetExtendedTextMessage().GetContextInfo().GetStanzaID()
	default:
		m.MessageType = "other"
	}

	isNew, err := in.store.Save(ctx, m, atts)
	if err != nil {
		return err
	}
	if isNew {
		in.log.Infof("Stored %s %s message %s in %s from %s: %q", source, m.MessageType, m.MessageID, g.Name, m.Sender, truncate(m.Body, 80))
	}
	for _, a := range atts {
		if a.Error == "" {
			in.log.Infof("Saved %s media file of message %s: %s", a.MediaType, m.MessageID, a.RelativePath)
		}
	}
	return nil
}

// media is a downloadable file carried by a message.
type media struct {
	file     whatsmeow.DownloadableMessage
	kind     string // image | video | audio | voice | document | sticker
	mime     string
	caption  string
	fileName string
	context  *waE2E.ContextInfo
}

func mediaOf(msg *waE2E.Message) *media {
	switch {
	case msg.GetImageMessage() != nil:
		x := msg.GetImageMessage()
		return &media{x, "image", x.GetMimetype(), x.GetCaption(), "", x.GetContextInfo()}
	case msg.GetVideoMessage() != nil:
		x := msg.GetVideoMessage()
		return &media{x, "video", x.GetMimetype(), x.GetCaption(), "", x.GetContextInfo()}
	case msg.GetAudioMessage() != nil:
		x := msg.GetAudioMessage()
		kind := "audio"
		if x.GetPTT() {
			kind = "voice"
		}
		return &media{x, kind, x.GetMimetype(), "", "", x.GetContextInfo()}
	case msg.GetDocumentMessage() != nil:
		x := msg.GetDocumentMessage()
		return &media{x, "document", x.GetMimetype(), x.GetCaption(), x.GetFileName(), x.GetContextInfo()}
	case msg.GetStickerMessage() != nil:
		x := msg.GetStickerMessage()
		return &media{x, "sticker", x.GetMimetype(), "", "", x.GetContextInfo()}
	}
	return nil
}

// albumParent returns the ID of the album message that msg is an item of.
func albumParent(msg *waE2E.Message) (parentID string, index int, ok bool) {
	assoc := msg.GetMessageContextInfo().GetMessageAssociation()
	if assoc.GetAssociationType() != waE2E.MessageAssociation_MEDIA_ALBUM {
		return "", 0, false
	}
	parentID = assoc.GetParentMessageKey().GetID()
	return parentID, int(assoc.GetMessageIndex()), parentID != ""
}

// saveMedia downloads md into folder, a folder of the media dir.
func (in *Ingester) saveMedia(ctx context.Context, md *media, info types.MessageInfo, folder string) db.MediaFile {
	att := db.MediaFile{
		SourceMessageID: info.ID,
		MediaType:       md.kind,
		Caption:         md.caption,
		FileName:        md.fileName,
		Mime:            md.mime,
	}
	data, err := in.client.Download(ctx, md.file)
	if err != nil {
		att.Error = err.Error()
		in.log.Warnf("Failed to download %s %s: %v", md.kind, info.ID, err)
		return att
	}
	rel := mediaRelPath(folder, info, md)
	dst := filepath.Join(in.mediaDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		att.Error = err.Error()
		in.log.Errorf("Failed to create directory for %s: %v", dst, err)
		return att
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		att.Error = err.Error()
		in.log.Errorf("Failed to write %s: %v", dst, err)
		return att
	}
	sum := sha256.Sum256(data)
	att.RelativePath = rel
	att.Size = int64(len(data))
	att.SHA256 = hex.EncodeToString(sum[:])
	return att
}

// mediaRelPath is where a file is saved, relative to the media dir and with
// "/" separators: <folder>/<UTC time>_<message id><ext>.
func mediaRelPath(folder string, info types.MessageInfo, md *media) string {
	name := fmt.Sprintf("%s_%s%s", info.Timestamp.UTC().Format("20060102T150405Z"), info.ID, extFor(md.mime, md.fileName))
	return path.Join(folder, name)
}

func textOf(msg *waE2E.Message) string {
	if msg == nil {
		return ""
	}
	if t := msg.GetConversation(); t != "" {
		return t
	}
	if t := msg.GetExtendedTextMessage().GetText(); t != "" {
		return t
	}
	if md := mediaOf(msg); md != nil {
		return md.caption
	}
	return ""
}

// extFor picks a file extension, preferring a document's own file name.
func extFor(mimetype, fileName string) string {
	if ext := filepath.Ext(fileName); ext != "" {
		return strings.ToLower(ext)
	}
	base, _, _ := strings.Cut(mimetype, ";")
	switch strings.TrimSpace(base) {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "audio/ogg":
		return ".ogg"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4":
		return ".m4a"
	case "application/pdf":
		return ".pdf"
	}
	if exts, _ := mime.ExtensionsByType(base); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

func truncate(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

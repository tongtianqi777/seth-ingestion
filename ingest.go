package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"os"
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

// Ingester filters messages from the target contact and persists them.
type Ingester struct {
	client      *whatsmeow.Client
	store       *db.Store
	targetPhone string // digits only, e.g. 14125778174
	targetName  string // stored in messages.sender
	mediaDir    string // images and other media are saved here
	log         waLog.Logger

	mu      sync.RWMutex // guards stopped against concurrent enqueue during Stop
	stopped bool
	queue   chan job
	done    chan struct{}
}

type job struct {
	evt    *events.Message
	source string
}

func NewIngester(client *whatsmeow.Client, store *db.Store, targetPhone, targetName, mediaDir string, log waLog.Logger) *Ingester {
	return &Ingester{
		client:      client,
		store:       store,
		targetPhone: targetPhone,
		targetName:  targetName,
		mediaDir:    mediaDir,
		log:         log,
		queue:       make(chan job, 1024),
		done:        make(chan struct{}),
	}
}

// Run processes queued messages on a single worker so that media downloads
// never block whatsmeow's event loop. It returns after Stop drains the queue.
func (in *Ingester) Run(ctx context.Context) {
	defer close(in.done)
	for j := range in.queue {
		if err := in.process(ctx, j.evt, j.source); err != nil {
			in.log.Errorf("Failed to ingest message %s: %v", j.evt.Info.ID, err)
		}
	}
}

func (in *Ingester) Stop() {
	in.mu.Lock()
	in.stopped = true
	close(in.queue)
	in.mu.Unlock()
	<-in.done
}

// HandleEvent is registered with whatsmeow's AddEventHandler.
func (in *Ingester) HandleEvent(rawEvt any) {
	switch evt := rawEvt.(type) {
	case *events.Message:
		in.enqueue(evt, "live")
	case *events.HistorySync:
		in.handleHistorySync(evt)
	}
}

func (in *Ingester) handleHistorySync(evt *events.HistorySync) {
	for _, conv := range evt.Data.GetConversations() {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil || !isDirectChat(chatJID) {
			continue
		}
		for _, hm := range conv.GetMessages() {
			msg, err := in.client.ParseWebMessage(chatJID, hm.GetMessage())
			if err != nil {
				continue
			}
			in.enqueue(msg, "history")
		}
	}
}

func (in *Ingester) enqueue(evt *events.Message, source string) {
	if evt.Info.IsFromMe || evt.Info.IsGroup || !isDirectChat(evt.Info.Chat) {
		return
	}
	if _, ok := in.targetSender(evt.Info.MessageSource); !ok {
		return
	}
	in.mu.RLock()
	defer in.mu.RUnlock()
	if !in.stopped {
		in.queue <- job{evt: evt, source: source}
	}
}

// isDirectChat reports whether jid is a one-to-one chat, as opposed to a
// group, broadcast list, status update or newsletter.
func isDirectChat(jid types.JID) bool {
	return jid.Server == types.DefaultUserServer || jid.Server == types.HiddenUserServer
}

// targetSender returns the sender's phone number if the message came from the
// target contact. WhatsApp may address users by phone JID or by an opaque LID,
// so both the primary and alternate sender JIDs are checked, and LIDs are
// resolved to phone numbers through whatsmeow's LID mapping store.
func (in *Ingester) targetSender(src types.MessageSource) (string, bool) {
	for _, jid := range []types.JID{src.Sender, src.SenderAlt} {
		if phone := in.phoneFor(jid); phone == in.targetPhone {
			return phone, true
		}
	}
	return "", false
}

func (in *Ingester) phoneFor(jid types.JID) string {
	if jid.IsEmpty() {
		return ""
	}
	jid = jid.ToNonAD()
	switch jid.Server {
	case types.DefaultUserServer:
		return jid.User
	case types.HiddenUserServer:
		pn, err := in.client.Store.LIDs.GetPNForLID(context.Background(), jid)
		if err == nil && !pn.IsEmpty() {
			return pn.User
		}
	}
	return ""
}

func (in *Ingester) process(ctx context.Context, evt *events.Message, source string) error {
	info := evt.Info
	msg := evt.Message
	// whatsmeow doesn't unwrap album items, which may arrive wrapped in an AssociatedChildMessage.
	if child := msg.GetAssociatedChildMessage().GetMessage(); child != nil {
		if child.MessageContextInfo == nil {
			child.MessageContextInfo = msg.MessageContextInfo
		}
		msg = child
	}

	phone, _ := in.targetSender(info.MessageSource)
	m := &db.Message{
		ChatJID:     info.Chat.String(),
		MessageID:   info.ID,
		Sender:      in.targetName,
		SenderJID:   info.Sender.ToNonAD().String(),
		SenderPhone: phone,
		SenderName:  info.PushName,
		IsGroup:     info.IsGroup,
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
		att := in.saveMedia(ctx, md, info)
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
		in.log.Infof("Stored %s %s message %s from %s: %q", source, m.MessageType, m.MessageID, info.PushName, truncate(m.Body, 80))
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

func (in *Ingester) saveMedia(ctx context.Context, md *media, info types.MessageInfo) db.MediaFile {
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
	// rel is relative to mediaDir and may include subdirectories in future.
	rel := fmt.Sprintf("%s_%s%s", info.Timestamp.UTC().Format("20060102T150405Z"), info.ID, extFor(md.mime, md.fileName))
	path := filepath.Join(in.mediaDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		att.Error = err.Error()
		in.log.Errorf("Failed to create directory for %s: %v", path, err)
		return att
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		att.Error = err.Error()
		in.log.Errorf("Failed to write %s: %v", path, err)
		return att
	}
	sum := sha256.Sum256(data)
	att.RelativePath = rel
	att.Size = int64(len(data))
	att.SHA256 = hex.EncodeToString(sum[:])
	return att
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

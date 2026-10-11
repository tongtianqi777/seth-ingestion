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
	imageDir    string
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

func NewIngester(client *whatsmeow.Client, store *db.Store, targetPhone, targetName, imageDir string, log waLog.Logger) *Ingester {
	return &Ingester{
		client:      client,
		store:       store,
		targetPhone: targetPhone,
		targetName:  targetName,
		imageDir:    imageDir,
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
	exists, err := in.store.Exists(ctx, info.Chat.String(), info.ID)
	if err != nil {
		return err
	}
	if exists {
		return nil
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
	if raw, err := protojson.Marshal(evt.Message); err == nil {
		m.RawJSON = string(raw)
	}

	msg := evt.Message
	switch {
	case msg.GetProtocolMessage().GetType() == waE2E.ProtocolMessage_MESSAGE_EDIT:
		pm := msg.GetProtocolMessage()
		m.MessageType = "edit"
		m.EditTargetID = pm.GetKey().GetID()
		m.Body = textOf(pm.GetEditedMessage())
	case msg.GetImageMessage() != nil:
		img := msg.GetImageMessage()
		m.MessageType = "image"
		m.Body = img.GetCaption()
		m.QuotedID = img.GetContextInfo().GetStanzaID()
		m.MediaMime = img.GetMimetype()
		in.saveImage(ctx, img, info, m)
	case msg.GetConversation() != "" || msg.GetExtendedTextMessage() != nil:
		m.MessageType = "text"
		m.Body = textOf(msg)
		m.QuotedID = msg.GetExtendedTextMessage().GetContextInfo().GetStanzaID()
	default:
		m.MessageType = "other"
	}

	inserted, err := in.store.Insert(ctx, m)
	if err != nil {
		return err
	}
	if inserted {
		in.log.Infof("Stored %s %s message %s from %s: %q", source, m.MessageType, m.MessageID, info.PushName, truncate(m.Body, 80))
	}
	return nil
}

func (in *Ingester) saveImage(ctx context.Context, img *waE2E.ImageMessage, info types.MessageInfo, m *db.Message) {
	data, err := in.client.Download(ctx, img)
	if err != nil {
		m.MediaError = err.Error()
		in.log.Warnf("Failed to download image %s: %v", info.ID, err)
		return
	}
	name := fmt.Sprintf("%s_%s%s", info.Timestamp.UTC().Format("20060102T150405Z"), info.ID, extFor(img.GetMimetype()))
	path := filepath.Join(in.imageDir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		m.MediaError = err.Error()
		in.log.Errorf("Failed to write image %s: %v", path, err)
		return
	}
	sum := sha256.Sum256(data)
	m.MediaPath = path
	m.MediaSize = int64(len(data))
	m.MediaSHA256 = hex.EncodeToString(sum[:])
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
	return msg.GetImageMessage().GetCaption()
}

func extFor(mimetype string) string {
	base, _, _ := strings.Cut(mimetype, ";")
	switch base {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
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

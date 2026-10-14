package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"github.com/tianqitong/seth-ingestion/internal/grouplist"
)

// reader runs read-only queries against the message database.
type reader struct {
	db *sql.DB
}

// openReader opens the message database read-only. seth-ingestion (or
// seth-sim) is the only writer; this connection can't change anything.
func openReader(path string) (*reader, error) {
	conn, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(8)
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	var tables int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('messages', 'chat_groups', 'message_media_files')`).Scan(&tables); err != nil || tables != 3 {
		conn.Close()
		return nil, fmt.Errorf("%s isn't a seth-ingestion message database", path)
	}
	return &reader{db: conn}, nil
}

func (r *reader) close() error { return r.db.Close() }

// hasTable reports whether the database has a table, such as the simulator's
// "simulation" and "simulated_events".
func (r *reader) hasTable(ctx context.Context, name string) bool {
	var ok bool
	r.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)`, name).Scan(&ok)
	return ok
}

// Group is one row of chat_groups, plus a summary of its messages.
type Group struct {
	GroupJID       string  `json:"group_jid"`
	Name           string  `json:"name"`
	CommunityJID   *string `json:"community_jid"`
	CommunityName  *string `json:"community_name"`
	IsAnnouncement bool    `json:"is_announcement"`
	MediaFolder    string  `json:"media_folder"`
	FirstSeenAt    string  `json:"first_seen_at"`
	UpdatedAt      string  `json:"updated_at"`
	MessageCount   int     `json:"message_count"`
	LastMessageAt  *string `json:"last_message_at"`
}

func (r *reader) groups(ctx context.Context) ([]Group, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT g.group_jid, g.name, g.community_jid, g.community_name, g.is_announcement, g.media_folder,
		       g.first_seen_at, g.updated_at,
		       (SELECT COUNT(*) FROM messages m WHERE m.chat_jid = g.group_jid),
		       (SELECT MAX(sent_at) FROM messages m WHERE m.chat_jid = g.group_jid)
		FROM chat_groups g
		ORDER BY g.is_announcement DESC, g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := []Group{}
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.GroupJID, &g.Name, &g.CommunityJID, &g.CommunityName, &g.IsAnnouncement, &g.MediaFolder,
			&g.FirstSeenAt, &g.UpdatedAt, &g.MessageCount, &g.LastMessageAt); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

var errNoGroup = errors.New("no such group")

// groupJID finds a group by its JID, its name (ignoring case and extra
// spaces, as in groups.txt) or its media folder, such as "preschool-pe-teachers".
func (r *reader) groupJID(ctx context.Context, group string) (string, error) {
	groups, err := r.groups(ctx)
	if err != nil {
		return "", err
	}
	for _, g := range groups {
		if g.GroupJID == group || grouplist.Key(g.Name) == grouplist.Key(group) || strings.EqualFold(g.MediaFolder, group) {
			return g.GroupJID, nil
		}
	}
	return "", errNoGroup
}

// Message is one row of messages, as an agent should read it: its group's
// name, its files in album order, and the latest edit applied in current_body.
type Message struct {
	Seq            int64            `json:"seq"` // messages.rowid
	ChatJID        string           `json:"chat_jid"`
	MessageID      string           `json:"message_id"`
	Group          string           `json:"group"`
	Sender         string           `json:"sender"`
	SenderJID      string           `json:"sender_jid"`
	SenderPhone    *string          `json:"sender_phone"`
	SenderName     *string          `json:"sender_name"`
	SentAt         string           `json:"sent_at"`
	IngestedAt     string           `json:"ingested_at"`
	MessageType    string           `json:"message_type"`
	Body           *string          `json:"body"`
	CurrentBody    *string          `json:"current_body,omitempty"` // body with the latest edit applied; not on edit rows
	Edited         bool             `json:"edited"`
	EditedAt       *string          `json:"edited_at,omitempty"`
	EditTargetID   *string          `json:"edit_target_id"`
	QuotedID       *string          `json:"quoted_id"`
	MediaFileCount int              `json:"media_file_count"`
	Source         string           `json:"source"`
	Files          []File           `json:"files"`
	RawJSON        *json.RawMessage `json:"raw_json,omitempty"`
	Quoted         *Message         `json:"quoted,omitempty"`  // the message replied to, on GET /messages/{chat_jid}/{message_id}
	Updated        bool             `json:"updated,omitempty"` // on /messages/new: an earlier message that gained files
}

// File is one row of message_media_files. URL is where GET /media/{id}
// serves it, or null if the download failed.
type File struct {
	ID              int64   `json:"id"`
	SourceMessageID string  `json:"source_message_id"`
	Position        int     `json:"position"`
	MediaType       string  `json:"media_type"`
	Caption         *string `json:"caption"`
	FileName        *string `json:"file_name"`
	RelativePath    *string `json:"relative_path"`
	Mime            *string `json:"mime"`
	Size            *int64  `json:"size"`
	SHA256          *string `json:"sha256"`
	Error           *string `json:"error"`
	URL             *string `json:"url"`
}

// messageQuery selects messages with the conditions in where, which may refer
// to m (messages) and g (chat_groups). Messages outside the recorded groups
// (rows from the earlier single-contact version) are left out, as the joins
// in CONSUMING.md leave them out.
type messageQuery struct {
	where   []string
	args    []any
	orderBy string
	limit   int
	raw     bool
}

func (q *messageQuery) add(cond string, args ...any) {
	q.where = append(q.where, cond)
	q.args = append(q.args, args...)
}

const messageColumns = `
	m.rowid, m.chat_jid, m.message_id, g.name, m.sender, m.sender_jid, m.sender_phone, m.sender_name,
	m.sent_at, m.ingested_at, m.message_type, m.body, m.edit_target_id, m.quoted_id, m.media_file_count,
	m.source, m.raw_json,
	(SELECT e.body FROM messages e WHERE e.chat_jid = m.chat_jid AND e.message_type = 'edit' AND e.edit_target_id = m.message_id
	 ORDER BY e.sent_at DESC, e.rowid DESC LIMIT 1),
	(SELECT e.sent_at FROM messages e WHERE e.chat_jid = m.chat_jid AND e.message_type = 'edit' AND e.edit_target_id = m.message_id
	 ORDER BY e.sent_at DESC, e.rowid DESC LIMIT 1)`

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (r *reader) messages(ctx context.Context, q querier, mq messageQuery) ([]Message, error) {
	query := "SELECT " + messageColumns + "\nFROM messages m JOIN chat_groups g ON g.group_jid = m.chat_jid"
	if len(mq.where) > 0 {
		query += "\nWHERE " + strings.Join(mq.where, " AND ")
	}
	if mq.orderBy == "" {
		mq.orderBy = "m.rowid"
	}
	query += "\nORDER BY " + mq.orderBy
	if mq.limit > 0 {
		query += "\nLIMIT " + strconv.Itoa(mq.limit)
	}
	rows, err := q.QueryContext(ctx, query, mq.args...)
	if err != nil {
		return nil, err
	}
	msgs := []Message{}
	for rows.Next() {
		var m Message
		var raw, latestEdit, editedAt sql.NullString
		if err := rows.Scan(&m.Seq, &m.ChatJID, &m.MessageID, &m.Group, &m.Sender, &m.SenderJID, &m.SenderPhone, &m.SenderName,
			&m.SentAt, &m.IngestedAt, &m.MessageType, &m.Body, &m.EditTargetID, &m.QuotedID, &m.MediaFileCount,
			&m.Source, &raw, &latestEdit, &editedAt); err != nil {
			rows.Close()
			return nil, err
		}
		if m.MessageType != "edit" {
			m.CurrentBody = m.Body
			if latestEdit.Valid {
				m.CurrentBody, m.Edited, m.EditedAt = &latestEdit.String, true, &editedAt.String
			}
		}
		if mq.raw && raw.Valid && json.Valid([]byte(raw.String)) {
			r := json.RawMessage(raw.String)
			m.RawJSON = &r
		}
		m.Files = []File{}
		msgs = append(msgs, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return msgs, r.attachFiles(ctx, q, msgs)
}

// attachFiles fills in each message's files, in album order.
func (r *reader) attachFiles(ctx context.Context, q querier, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	byKey := map[[2]string]*Message{}
	values := make([]string, 0, len(msgs))
	args := make([]any, 0, 2*len(msgs))
	for i := range msgs {
		key := [2]string{msgs[i].ChatJID, msgs[i].MessageID}
		if byKey[key] == nil && msgs[i].MediaFileCount > 0 {
			byKey[key] = &msgs[i]
			values = append(values, "(?, ?)")
			args = append(args, key[0], key[1])
		}
	}
	if len(values) == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT chat_jid, message_id, id, source_message_id, position, media_type, caption, file_name,
		       relative_path, mime, size, sha256, error
		FROM message_media_files
		WHERE (chat_jid, message_id) IN (VALUES `+strings.Join(values, ", ")+`)
		ORDER BY position, id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var chat, msgID string
		var f File
		if err := rows.Scan(&chat, &msgID, &f.ID, &f.SourceMessageID, &f.Position, &f.MediaType, &f.Caption, &f.FileName,
			&f.RelativePath, &f.Mime, &f.Size, &f.SHA256, &f.Error); err != nil {
			return err
		}
		if f.RelativePath != nil {
			u := "/media/" + strconv.FormatInt(f.ID, 10)
			f.URL = &u
		}
		m := byKey[[2]string{chat, msgID}]
		m.Files = append(m.Files, f)
	}
	// Other rows sharing the key (repeated in one result) get the same files.
	for i := range msgs {
		if m := byKey[[2]string{msgs[i].ChatJID, msgs[i].MessageID}]; m != nil && m != &msgs[i] {
			msgs[i].Files = m.Files
		}
	}
	return rows.Err()
}

// message returns one message, with the message it quotes.
func (r *reader) message(ctx context.Context, chatJID, messageID string) (*Message, error) {
	mq := messageQuery{raw: true}
	mq.add("m.chat_jid = ? AND m.message_id = ?", chatJID, messageID)
	msgs, err := r.messages(ctx, r.db, mq)
	if err != nil || len(msgs) == 0 {
		return nil, err
	}
	m := msgs[0]
	if m.QuotedID != nil {
		qq := messageQuery{}
		qq.add("m.chat_jid = ? AND m.message_id = ?", chatJID, *m.QuotedID)
		if quoted, err := r.messages(ctx, r.db, qq); err == nil && len(quoted) > 0 {
			m.Quoted = &quoted[0]
		}
	}
	return &m, nil
}

// mediaFile returns where a file was saved and its type. ok is false if
// there's no such file or its download failed.
func (r *reader) mediaFile(ctx context.Context, id int64) (rel, mime, fileName string, ok bool, err error) {
	var relPath, mimeType, name sql.NullString
	err = r.db.QueryRowContext(ctx, `SELECT relative_path, mime, file_name FROM message_media_files WHERE id = ?`, id).
		Scan(&relPath, &mimeType, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", false, nil
	}
	if err != nil || !relPath.Valid {
		return "", "", "", false, err
	}
	return relPath.String, mimeType.String, name.String, true, nil
}

// SimulatedEvent is one row of the simulator's simulated_events table: what
// an event announcement really means.
type SimulatedEvent struct {
	ChatJID          string  `json:"chat_jid"`
	MessageID        string  `json:"message_id"`
	Seq              int64   `json:"seq"`
	Group            string  `json:"group"`
	SentAt           string  `json:"sent_at"`
	MessageType      string  `json:"message_type"`
	Body             *string `json:"body"`
	Event            string  `json:"event"`
	Action           string  `json:"action"`
	StartsAt         string  `json:"starts_at"`
	EndsOn           *string `json:"ends_on"`
	AllDay           bool    `json:"all_day"`
	PreviousStartsAt *string `json:"previous_starts_at"`
	DeadlineOn       *string `json:"deadline_on"`
}

func (r *reader) simulatedEvents(ctx context.Context, since int64, action string, limit int) ([]SimulatedEvent, error) {
	query := `
		SELECT e.chat_jid, e.message_id, m.rowid, g.name, m.sent_at, m.message_type, m.body, e.event, e.action,
		       e.starts_at, e.ends_on, e.all_day, e.previous_starts_at, e.deadline_on
		FROM simulated_events e
		JOIN messages m USING (chat_jid, message_id)
		JOIN chat_groups g ON g.group_jid = m.chat_jid
		WHERE m.rowid > ?`
	args := []any{since}
	if action != "" {
		query += " AND e.action = ?"
		args = append(args, action)
	}
	query += " ORDER BY m.rowid LIMIT " + strconv.Itoa(limit)
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []SimulatedEvent{}
	for rows.Next() {
		var e SimulatedEvent
		if err := rows.Scan(&e.ChatJID, &e.MessageID, &e.Seq, &e.Group, &e.SentAt, &e.MessageType, &e.Body, &e.Event, &e.Action,
			&e.StartsAt, &e.EndsOn, &e.AllDay, &e.PreviousStartsAt, &e.DeadlineOn); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

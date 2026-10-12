// Package db is the local SQLite store for ingested messages.
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schema string

type Message struct {
	ChatJID      string
	MessageID    string
	Sender       string
	SenderJID    string
	SenderPhone  string
	SenderName   string
	IsGroup      bool
	SentAt       time.Time
	MessageType  string
	Body         string
	EditTargetID string
	QuotedID     string
	Source       string
	RawJSON      string
}

// MediaFile is one media file belonging to a Message.
type MediaFile struct {
	SourceMessageID string
	Position        int
	MediaType       string
	Caption         string
	FileName        string
	RelativePath    string
	Mime            string
	Size            int64
	SHA256          string
	Error           string
}

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000", path))
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// migrate adds columns to a messages table created by an older version.
// It runs before schema.sql, whose indexes may reference the new columns.
func migrate(ctx context.Context, db *sql.DB) error {
	cols, err := columns(ctx, db, "messages")
	if err != nil || len(cols) == 0 {
		return err // len 0: fresh database; schema.sql creates everything
	}
	added := map[string]string{
		"sender":           `ALTER TABLE messages ADD COLUMN sender TEXT NOT NULL DEFAULT ''`,
		"media_file_count": `ALTER TABLE messages ADD COLUMN media_file_count INTEGER NOT NULL DEFAULT 0`,
	}
	for col, stmt := range added {
		if !cols[col] {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
	}
	return nil
}

func columns(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, table string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// BackfillSender sets sender on rows from phone that predate the sender column.
func (s *Store) BackfillSender(ctx context.Context, phone, sender string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE messages SET sender = ? WHERE sender = '' AND sender_phone = ?`, sender, phone)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// legacyMediaColumns are the columns on messages that held a single media file
// before media files moved to the message_media_files table.
var legacyMediaColumns = []string{"media_path", "image_relative_path", "media_mime", "media_size", "media_sha256", "media_error"}

// MigrateLegacyMedia moves media stored on messages rows by older versions into
// message_media_files, then drops the old columns. mediaDir is used to derive
// relative paths for rows that only recorded an absolute media_path.
func (s *Store) MigrateLegacyMedia(ctx context.Context, mediaDir string) (int, error) {
	cols, err := columns(ctx, s.db, "messages")
	if err != nil || !cols["media_path"] {
		return 0, err
	}
	relCol := "NULL"
	if cols["image_relative_path"] {
		relCol = "image_relative_path"
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT chat_jid, message_id, body, media_path, `+relCol+`, media_mime, media_size, media_sha256, media_error, ingested_at
		FROM messages WHERE media_path IS NOT NULL OR media_error IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	type legacy struct {
		chat, id, ingestedAt                string
		body, abs, rel, mime, sha, mediaErr sql.NullString
		size                                sql.NullInt64
	}
	var found []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.chat, &l.id, &l.body, &l.abs, &l.rel, &l.mime, &l.size, &l.sha, &l.mediaErr, &l.ingestedAt); err != nil {
			rows.Close()
			return 0, err
		}
		found = append(found, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, l := range found {
		rel := l.rel.String
		if rel == "" && l.abs.Valid {
			if r, err := filepath.Rel(mediaDir, l.abs.String); err == nil && r != ".." && !strings.HasPrefix(r, "../") {
				rel = filepath.ToSlash(r)
			}
		}
		// Older versions only saved images.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO message_media_files (
				chat_jid, message_id, source_message_id, position, media_type, caption,
				relative_path, mime, size, sha256, error, ingested_at
			) VALUES (?, ?, ?, 0, 'image', ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (chat_jid, source_message_id) DO NOTHING`,
			l.chat, l.id, l.id, l.body, nullable(rel), l.mime, l.size, l.sha, l.mediaErr, l.ingestedAt,
		); err != nil {
			return 0, err
		}
	}
	for _, col := range legacyMediaColumns {
		if cols[col] {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE messages DROP COLUMN `+col); err != nil {
				return 0, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE messages SET media_file_count = (
			SELECT COUNT(*) FROM message_media_files a
			WHERE a.chat_jid = messages.chat_jid AND a.message_id = messages.message_id)`); err != nil {
		return 0, err
	}
	return len(found), tx.Commit()
}

// MediaFileExists reports whether a file carried by WhatsApp message
// sourceMessageID is already stored, so re-synced history isn't downloaded twice.
func (s *Store) MediaFileExists(ctx context.Context, chatJID, sourceMessageID string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM message_media_files WHERE chat_jid = ? AND source_message_id = ?)`,
		chatJID, sourceMessageID).Scan(&exists)
	return exists, err
}

// Save stores a message and its media files in one transaction and reports
// whether the message row is new.
//
// If the message row already exists (a duplicate from history sync, or an
// album whose row was created by an earlier item), it is kept, and only
// missing body/raw_json values and an earlier sent_at are filled in. New
// media files are added either way, and media_file_count is recomputed.
func (s *Store) Save(ctx context.Context, m *Message, atts []MediaFile) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	sentAt := m.SentAt.UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO messages (
			chat_jid, message_id, sender, sender_jid, sender_phone, sender_name, is_group,
			sent_at, ingested_at, message_type, body, edit_target_id, quoted_id, source, raw_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (chat_jid, message_id) DO NOTHING`,
		m.ChatJID, m.MessageID, m.Sender, m.SenderJID, nullable(m.SenderPhone), nullable(m.SenderName), m.IsGroup,
		sentAt, now, m.MessageType, nullable(m.Body), nullable(m.EditTargetID), nullable(m.QuotedID),
		m.Source, nullable(m.RawJSON),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	isNew := n > 0
	if !isNew {
		if _, err := tx.ExecContext(ctx, `
			UPDATE messages SET
				body     = COALESCE(body, ?),
				raw_json = COALESCE(raw_json, ?),
				sent_at  = MIN(sent_at, ?)
			WHERE chat_jid = ? AND message_id = ?`,
			nullable(m.Body), nullable(m.RawJSON), sentAt, m.ChatJID, m.MessageID); err != nil {
			return false, err
		}
	}

	for _, a := range atts {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO message_media_files (
				chat_jid, message_id, source_message_id, position, media_type, caption, file_name,
				relative_path, mime, size, sha256, error, ingested_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (chat_jid, source_message_id) DO NOTHING`,
			m.ChatJID, m.MessageID, a.SourceMessageID, a.Position, a.MediaType, nullable(a.Caption), nullable(a.FileName),
			nullable(a.RelativePath), nullable(a.Mime), nullableInt(a.Size), nullable(a.SHA256), nullable(a.Error), now,
		); err != nil {
			return false, err
		}
	}
	if len(atts) > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE messages SET media_file_count = (
				SELECT COUNT(*) FROM message_media_files
				WHERE chat_jid = messages.chat_jid AND message_id = messages.message_id)
			WHERE chat_jid = ? AND message_id = ?`, m.ChatJID, m.MessageID); err != nil {
			return false, err
		}
	}
	return isNew, tx.Commit()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

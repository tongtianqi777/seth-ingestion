// Package db is the local SQLite store for ingested messages.
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
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
	MediaPath    string
	MediaMime    string
	MediaSize    int64
	MediaSHA256  string
	MediaError   string
	Source       string
	RawJSON      string
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

// migrate brings a messages table created by an older version up to date.
// It runs before schema.sql, whose indexes may reference the new columns.
func migrate(ctx context.Context, db *sql.DB) error {
	cols := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('messages')`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(cols) == 0 {
		return nil // fresh database; schema.sql creates everything
	}
	if !cols["sender"] {
		if _, err := db.ExecContext(ctx, `ALTER TABLE messages ADD COLUMN sender TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
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

// Exists reports whether a message is already stored, so history re-syncs don't re-download media.
func (s *Store) Exists(ctx context.Context, chatJID, messageID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE chat_jid = ? AND message_id = ?`, chatJID, messageID).Scan(&n)
	return n > 0, err
}

// Insert stores a message. Duplicates (same chat + message ID) are ignored.
func (s *Store) Insert(ctx context.Context, m *Message) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (
			chat_jid, message_id, sender, sender_jid, sender_phone, sender_name, is_group,
			sent_at, ingested_at, message_type, body, edit_target_id, quoted_id,
			media_path, media_mime, media_size, media_sha256, media_error, source, raw_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (chat_jid, message_id) DO NOTHING`,
		m.ChatJID, m.MessageID, m.Sender, m.SenderJID, nullable(m.SenderPhone), nullable(m.SenderName), m.IsGroup,
		m.SentAt.UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339),
		m.MessageType, nullable(m.Body), nullable(m.EditTargetID), nullable(m.QuotedID),
		nullable(m.MediaPath), nullable(m.MediaMime), nullableInt(m.MediaSize), nullable(m.MediaSHA256),
		nullable(m.MediaError), m.Source, nullable(m.RawJSON),
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
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

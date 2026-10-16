package db

import (
	"context"
	"database/sql"
	"errors"
)

// HDChild is a message that delivered the high-resolution copy of a file
// whose low-resolution copy came in an earlier message (WhatsApp's "HD
// quality" photos and videos are sent as such a pair).
type HDChild struct {
	ChatJID   string
	MessageID string // the HD message, which has its own messages row
	ParentID  string // the low-resolution message it belongs to
}

// hdChildren selects messages that carry a saved HD file on their own row.
// raw_json is the protobuf message as JSON, where the HD message names its parent.
const hdChildren = `
	SELECT m.chat_jid, m.message_id,
	       json_extract(m.raw_json, '$.messageContextInfo.messageAssociation.parentMessageKey.ID') AS parent_id
	FROM messages m
	JOIN message_media_files f ON f.chat_jid = m.chat_jid AND f.message_id = m.message_id
	                          AND f.source_message_id = m.message_id
	WHERE f.relative_path IS NOT NULL
	  AND json_extract(m.raw_json, '$.messageContextInfo.messageAssociation.associationType')
	      IN ('HD_IMAGE_DUAL_UPLOAD', 'HD_VIDEO_DUAL_UPLOAD')`

// HDChildren lists every stored HD message that has a saved file and a parent.
func (s *Store) HDChildren(ctx context.Context) ([]HDChild, error) {
	rows, err := s.db.QueryContext(ctx, hdChildren)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HDChild
	for rows.Next() {
		var c HDChild
		var parent sql.NullString
		if err := rows.Scan(&c.ChatJID, &c.MessageID, &parent); err != nil {
			return nil, err
		}
		if parent.String != "" {
			c.ParentID = parent.String
			out = append(out, c)
		}
	}
	return out, rows.Err()
}

// HDChildOf returns the ID of the stored HD message of parentID, if there is one.
func (s *Store) HDChildOf(ctx context.Context, chatJID, parentID string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT message_id FROM (`+hdChildren+`) WHERE chat_jid = ? AND parent_id = ?`, chatJID, parentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

// MediaFile returns the file delivered by WhatsApp message sourceMessageID, or
// nil if none is stored.
func (s *Store) MediaFile(ctx context.Context, chatJID, sourceMessageID string) (*MediaFile, error) {
	var f MediaFile
	var path, mime, errText, sum sql.NullString
	var size sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT source_message_id, position, media_type, relative_path, mime, size, sha256, error
		FROM message_media_files WHERE chat_jid = ? AND source_message_id = ?`, chatJID, sourceMessageID).
		Scan(&f.SourceMessageID, &f.Position, &f.MediaType, &path, &mime, &size, &sum, &errText)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.RelativePath, f.Mime, f.Size, f.SHA256, f.Error = path.String, mime.String, size.Int64, sum.String, errText.String
	return &f, nil
}

// ReplaceMediaFile swaps the saved file of message sourceMessageID for f, which
// keeps its place (message, position, caption). It returns the path of the
// file it replaced, and false if there is no such message.
func (s *Store) ReplaceMediaFile(ctx context.Context, chatJID, sourceMessageID string, f MediaFile) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var old sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT relative_path FROM message_media_files WHERE chat_jid = ? AND source_message_id = ?`,
		chatJID, sourceMessageID).Scan(&old)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE message_media_files
		SET media_type = ?, relative_path = ?, mime = ?, size = ?, sha256 = ?, error = ?
		WHERE chat_jid = ? AND source_message_id = ?`,
		f.MediaType, nullable(f.RelativePath), nullable(f.Mime), nullableInt(f.Size), nullable(f.SHA256), nullable(f.Error),
		chatJID, sourceMessageID); err != nil {
		return "", false, err
	}
	return old.String, true, tx.Commit()
}

// DeleteMessage removes a message and its media file rows. It doesn't delete the files.
func (s *Store) DeleteMessage(ctx context.Context, chatJID, messageID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE chat_jid = ? AND message_id = ?`, chatJID, messageID)
	return err
}

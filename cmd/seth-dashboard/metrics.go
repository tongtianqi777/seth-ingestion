package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/tianqitong/seth-ingestion/internal/readonly"
)

// Metrics summarizes what's in the message database, for the dashboard's
// at-a-glance panel. It's read straight from the database, so it works
// whether or not the services are running.
type Metrics struct {
	Available       bool           `json:"available"`         // false until the ingestion has created the database
	Database        string         `json:"database"`          // the full path read
	Messages        int            `json:"messages"`          // all messages in the recorded groups
	MessagesLast24h int            `json:"messages_last_24h"` // sent in the last 24 hours
	MediaFiles      int            `json:"media_files"`       // photos, videos, voice notes, documents…
	MediaFailed     int            `json:"media_failed"`      // of those, downloads that failed for any other reason
	MediaExpired    int            `json:"media_expired"`     // of those, files WhatsApp no longer keeps
	MediaBytes      int64          `json:"media_bytes"`       // total size of the saved files
	NewestSentAt    *string        `json:"newest_sent_at"`    // the most recent message's sent_at
	LastIngestedAt  *string        `json:"last_ingested_at"`  // when the last message was written
	Groups          []GroupMetrics `json:"groups"`
	Storage         Storage        `json:"storage"`
	ReadAt          string         `json:"read_at"`
}

// GroupMetrics is one recorded group's share.
type GroupMetrics struct {
	GroupJID       string  `json:"group_jid"`
	Name           string  `json:"name"`
	IsAnnouncement bool    `json:"is_announcement"`
	Messages       int     `json:"messages"`
	MediaFiles     int     `json:"media_files"`
	MediaFailed    int     `json:"media_failed"`
	MediaExpired   int     `json:"media_expired"`
	LastMessageAt  *string `json:"last_message_at"`
}

// SQL conditions on message_media_files rows without a saved file. WhatsApp
// answers 403, 404 or 410 for media it no longer keeps, which is normal for
// older history, so those are counted as expired rather than failed. The error
// text is whatsmeow's "download failed with status code N", stored by ingest.go.
const (
	isExpiredSQL = `f.error IN (
		'download failed with status code 403',
		'download failed with status code 404',
		'download failed with status code 410')`
	expiredSQL = `f.relative_path IS NULL AND ` + isExpiredSQL
	failedSQL  = `f.relative_path IS NULL AND NOT COALESCE(` + isExpiredSQL + `, 0)`
)

// readMetrics opens the database read-only, runs a few short queries in one
// read transaction (one consistent snapshot), and closes it again.
func readMetrics(ctx context.Context, path string) (Metrics, error) {
	m := Metrics{Database: path, Groups: []GroupMetrics{}, ReadAt: time.Now().UTC().Format(time.RFC3339)}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return m, nil // no data yet: the ingestion hasn't run
	}
	conn, err := readonly.Open(path, 2000)
	if err != nil {
		return m, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return m, err
	}
	defer tx.Rollback()

	since := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	// Only messages in the recorded groups count, as in seth-feed and CONSUMING.md.
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(m.sent_at >= ?), 0), MAX(m.sent_at), MAX(m.ingested_at)
		FROM messages m JOIN chat_groups g ON g.group_jid = m.chat_jid`, since).
		Scan(&m.Messages, &m.MessagesLast24h, &m.NewestSentAt, &m.LastIngestedAt); err != nil {
		return m, err
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(`+failedSQL+`), 0), COALESCE(SUM(`+expiredSQL+`), 0), COALESCE(SUM(f.size), 0)
		FROM message_media_files f JOIN chat_groups g ON g.group_jid = f.chat_jid`).
		Scan(&m.MediaFiles, &m.MediaFailed, &m.MediaExpired, &m.MediaBytes); err != nil {
		return m, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT g.group_jid, g.name, g.is_announcement,
		       (SELECT COUNT(*) FROM messages m WHERE m.chat_jid = g.group_jid),
		       (SELECT COUNT(*) FROM message_media_files f WHERE f.chat_jid = g.group_jid),
		       (SELECT COUNT(*) FROM message_media_files f WHERE f.chat_jid = g.group_jid AND `+failedSQL+`),
		       (SELECT COUNT(*) FROM message_media_files f WHERE f.chat_jid = g.group_jid AND `+expiredSQL+`),
		       (SELECT MAX(sent_at) FROM messages m WHERE m.chat_jid = g.group_jid)
		FROM chat_groups g
		ORDER BY g.is_announcement DESC, g.name`)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var g GroupMetrics
		if err := rows.Scan(&g.GroupJID, &g.Name, &g.IsAnnouncement, &g.Messages, &g.MediaFiles, &g.MediaFailed, &g.MediaExpired, &g.LastMessageAt); err != nil {
			return m, err
		}
		m.Groups = append(m.Groups, g)
	}
	if err := rows.Err(); err != nil {
		return m, err
	}
	m.Available = true
	return m, nil
}

// GET /api/metrics
func (d *dashboard) metrics(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	path := d.abs(d.config.dbPath())
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	m, err := readMetrics(ctx, path)
	d.mu.Lock()
	media, data := d.abs(d.config.MediaDir), d.abs(d.config.DataDir)
	d.mu.Unlock()
	m.Storage = d.storage.get(media, data) // also shown when the database can't be read
	if err != nil {
		// The ingestion may be busy writing; the page tries again on its next poll.
		d.log.Printf("metrics: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "couldn't read the database just now: " + err.Error(), "database": path, "storage": m.Storage})
		return
	}
	writeJSON(w, http.StatusOK, m)
}

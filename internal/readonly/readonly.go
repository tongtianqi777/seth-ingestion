// Package readonly opens seth-ingestion's message database for reading only,
// for the programs that serve or summarize it (seth-feed, seth-dashboard)
// while seth-ingestion writes to it.
package readonly

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

// Open opens the message database at path with mode=ro, so the connection
// can't change anything, and checks it has seth-ingestion's tables. Queries
// wait up to busyTimeout milliseconds for the writer instead of failing.
func Open(path string, busyTimeout int) (*sql.DB, error) {
	conn, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?mode=ro&_busy_timeout=%d", path, busyTimeout))
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
	return conn, nil
}

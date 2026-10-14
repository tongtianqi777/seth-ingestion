package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// server serves the message database as a read-only JSON API.
type server struct {
	db         *reader
	dbPath     string
	mediaDir   string
	corsOrigin string // "" to send no CORS headers
	log        *log.Logger
}

const (
	defaultLimit = 50
	maxLimit     = 500
	queryTimeout = 10 * time.Second
)

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /groups", s.listGroups)
	mux.HandleFunc("GET /groups/{group}/messages", s.listMessages)
	mux.HandleFunc("GET /messages", s.listMessages)
	mux.HandleFunc("GET /messages/new", s.newMessages)
	mux.HandleFunc("GET /messages/{chat_jid}/{message_id}", s.getMessage)
	mux.HandleFunc("GET /search", s.search)
	mux.HandleFunc("GET /media/{id}", s.media)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "seth-feed is read-only: only GET requests are accepted")
			return
		}
		writeError(w, http.StatusNotFound, "no such endpoint; see docs/CONSUMING.md#http-api")
	})
	return s.logRequests(s.cors(mux))
}

// statusWriter remembers the status code, for the request log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.log.Printf("%s %s %d %s", r.Method, r.URL.RequestURI(), sw.status, time.Since(start).Round(time.Millisecond))
	})
}

func (s *server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.corsOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", s.corsOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// fail reports an error from the database as a 500, and logs it.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusServiceUnavailable, "the query took too long; try a smaller limit or a narrower filter")
		return
	}
	s.log.Printf("error: %s: %v", r.URL.Path, err)
	writeError(w, http.StatusInternalServerError, "database error")
}

func withTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), queryTimeout)
}

// badRequest is an error in the request's parameters, reported as a 400.
type badRequest string

func (e badRequest) Error() string { return string(e) }

func (s *server) handleErr(w http.ResponseWriter, r *http.Request, err error) {
	var bad badRequest
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, string(bad))
	case errors.Is(err, errNoGroup):
		writeError(w, http.StatusNotFound, "no such group; GET /groups lists them")
	default:
		s.fail(w, r, err)
	}
}

func limitParam(r *http.Request, def, max int) (int, error) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, badRequest("limit must be a positive number")
	}
	if n > max {
		n = max
	}
	return n, nil
}

func intParam(r *http.Request, name string) (int64, bool, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, false, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, false, badRequest(name + " must be a non-negative number (a seq)")
	}
	return n, true, nil
}

func boolParam(r *http.Request, name string) bool {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// timeParam reads a time as RFC 3339 (2026-10-14T16:00:00Z) or a date
// (2026-10-14, midnight in the server's time zone), and returns it in the
// form sent_at uses, so it compares correctly as text.
func timeParam(r *http.Request, name string) (string, bool, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return "", false, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		if t, err = time.ParseInLocation("2006-01-02", v, time.Local); err != nil {
			return "", false, badRequest(name + " must be a date (2026-10-14) or an RFC 3339 time (2026-10-14T16:00:00Z)")
		}
	}
	return t.UTC().Format(time.RFC3339), true, nil
}

// groupFilter adds a group condition from the {group} path value or the
// group query parameter.
func (s *server) groupFilter(ctx context.Context, r *http.Request, mq *messageQuery) error {
	group := r.PathValue("group")
	if group == "" {
		group = r.URL.Query().Get("group")
	}
	if group == "" {
		return nil
	}
	jid, err := s.db.groupJID(ctx, group)
	if err != nil {
		return err
	}
	mq.add("m.chat_jid = ?", jid)
	return nil
}

func (s *server) timeFilter(r *http.Request, mq *messageQuery) error {
	from, ok, err := timeParam(r, "from")
	if err != nil {
		return err
	}
	if ok {
		mq.add("m.sent_at >= ?", from)
	}
	to, ok, err := timeParam(r, "to")
	if err != nil {
		return err
	}
	if ok {
		mq.add("m.sent_at < ?", to)
	}
	return nil
}

// GET /health
func (s *server) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	var messages, groups int
	if err := s.db.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM messages), (SELECT COUNT(*) FROM chat_groups)`).Scan(&messages, &groups); err != nil {
		s.fail(w, r, err)
		return
	}
	c, err := s.headCursor(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	_, mediaErr := os.Stat(s.mediaDir)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"simulated":        s.db.hasTable(ctx, "simulation"),
		"events_available": s.db.hasTable(ctx, "simulated_events"),
		"database":         s.dbPath,
		"media_dir":        s.mediaDir,
		"media_dir_exists": mediaErr == nil,
		"groups":           groups,
		"messages":         messages,
		"cursor":           c.String(),
	})
}

// GET /groups
func (s *server) listGroups(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	groups, err := s.db.groups(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

// GET /messages and GET /groups/{group}/messages
func (s *server) listMessages(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	if err := s.listMessagesErr(ctx, w, r); err != nil {
		s.handleErr(w, r, err)
	}
}

func (s *server) listMessagesErr(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	limit, err := limitParam(r, defaultLimit, maxLimit)
	if err != nil {
		return err
	}
	order := strings.ToLower(q.Get("order"))
	if order == "" {
		order = "desc"
	}
	if order != "asc" && order != "desc" {
		return badRequest("order must be asc or desc")
	}
	sortBy := strings.ToLower(q.Get("sort"))
	if sortBy == "" {
		sortBy = "seq"
	}
	if sortBy != "seq" && sortBy != "sent_at" {
		return badRequest("sort must be seq or sent_at")
	}
	mq := messageQuery{limit: limit + 1, raw: boolParam(r, "raw")}
	if err := s.groupFilter(ctx, r, &mq); err != nil {
		return err
	}
	if err := s.timeFilter(r, &mq); err != nil {
		return err
	}
	if !boolParam(r, "include_edits") {
		mq.add("m.message_type <> 'edit'")
	}
	since, hasSince, err := intParam(r, "since")
	if err != nil {
		return err
	}
	before, hasBefore, err := intParam(r, "before")
	if err != nil {
		return err
	}
	if (hasSince || hasBefore) && sortBy != "seq" {
		return badRequest("since and before page by seq, so they need sort=seq")
	}
	if hasSince {
		mq.add("m.rowid > ?", since)
	}
	if hasBefore {
		mq.add("m.rowid < ?", before)
	}
	if sortBy == "seq" {
		mq.orderBy = "m.rowid " + order
	} else {
		mq.orderBy = "m.sent_at " + order + ", m.rowid " + order
	}

	tx, err := s.db.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	msgs, err := s.db.messages(ctx, tx, mq)
	if err != nil {
		return err
	}
	hasMore := len(msgs) > limit
	if hasMore {
		msgs = msgs[:limit]
	}
	var newest int64
	for _, m := range msgs {
		newest = max(newest, m.Seq)
	}
	if newest == 0 {
		newest = since
	}
	c, err := cursorAt(ctx, tx, newest)
	if err != nil {
		return err
	}
	resp := map[string]any{"messages": msgs, "has_more": hasMore, "cursor": c.String()}
	if hasMore && sortBy == "seq" {
		last := msgs[len(msgs)-1].Seq
		if order == "desc" {
			resp["next"] = map[string]int64{"before": last}
		} else {
			resp["next"] = map[string]int64{"since": last}
		}
	}
	writeJSON(w, http.StatusOK, resp)
	return nil
}

// cursor marks how far a follower has read: the highest message seq
// (messages.rowid) it has, and the highest file id among those messages.
// The file part catches photos added to an album after its message was read.
type cursor struct {
	seq, file int64
}

func (c cursor) String() string { return fmt.Sprintf("%d.%d", c.seq, c.file) }

// parseCursor reads "SEQ.FILE". A bare "SEQ" (for example a seq from a
// message) is accepted too; its file part is worked out from the database.
func parseCursor(v string) (c cursor, needsFile bool, err error) {
	if v == "" {
		return cursor{}, false, nil
	}
	seqPart, filePart, hasFile := strings.Cut(v, ".")
	if c.seq, err = strconv.ParseInt(seqPart, 10, 64); err != nil || c.seq < 0 {
		return c, false, badRequest("since must be a cursor such as 1532.877, as returned in \"cursor\"")
	}
	if !hasFile {
		return c, true, nil
	}
	if c.file, err = strconv.ParseInt(filePart, 10, 64); err != nil || c.file < 0 {
		return c, false, badRequest("since must be a cursor such as 1532.877, as returned in \"cursor\"")
	}
	return c, false, nil
}

// cursorAt is the cursor for having read every message up to seq.
func cursorAt(ctx context.Context, q querier, seq int64) (cursor, error) {
	c := cursor{seq: seq}
	err := q.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(f.id), 0) FROM message_media_files f
		JOIN messages m ON m.chat_jid = f.chat_jid AND m.message_id = f.message_id
		WHERE m.rowid <= ?`, seq).Scan(&c.file)
	return c, err
}

// headCursor is the cursor for having read everything recorded so far.
func (s *server) headCursor(ctx context.Context) (cursor, error) {
	var seq int64
	if err := s.db.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid), 0) FROM messages`).Scan(&seq); err != nil {
		return cursor{}, err
	}
	return cursorAt(ctx, s.db.db, seq)
}

// GET /messages/new?since=CURSOR — the follow endpoint.
func (s *server) newMessages(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	if err := s.newMessagesErr(ctx, w, r); err != nil {
		s.handleErr(w, r, err)
	}
}

func (s *server) newMessagesErr(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	limit, err := limitParam(r, 100, maxLimit)
	if err != nil {
		return err
	}
	since, needsFile, err := parseCursor(r.URL.Query().Get("since"))
	if err != nil {
		return err
	}
	var filters messageQuery
	if err := s.groupFilter(ctx, r, &filters); err != nil {
		return err
	}

	// One read transaction, so the messages and the new cursor come from the
	// same snapshot and nothing written in between is skipped.
	tx, err := s.db.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if needsFile {
		if since, err = cursorAt(ctx, tx, since.seq); err != nil {
			return err
		}
	}
	raw := boolParam(r, "raw")

	// Messages read before that have since gained files: album photos that
	// arrived after the album's first photo.
	updated := messageQuery{raw: raw, limit: maxLimit}
	updated.where = append([]string{}, filters.where...)
	updated.args = append([]any{}, filters.args...)
	updated.add(`m.rowid <= ? AND EXISTS (SELECT 1 FROM message_media_files f
		WHERE f.chat_jid = m.chat_jid AND f.message_id = m.message_id AND f.id > ?)`, since.seq, since.file)
	changed, err := s.db.messages(ctx, tx, updated)
	if err != nil {
		return err
	}
	for i := range changed {
		changed[i].Updated = true
	}

	fresh := messageQuery{raw: raw, limit: limit + 1}
	fresh.where = append([]string{}, filters.where...)
	fresh.args = append([]any{}, filters.args...)
	fresh.add("m.rowid > ?", since.seq)
	msgs, err := s.db.messages(ctx, tx, fresh)
	if err != nil {
		return err
	}
	hasMore := len(msgs) > limit
	if hasMore {
		msgs = msgs[:limit]
	}
	next := since.seq
	if len(msgs) > 0 {
		next = msgs[len(msgs)-1].Seq
	}
	c, err := cursorAt(ctx, tx, next)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": append(changed, msgs...),
		"cursor":   c.String(),
		"has_more": hasMore,
	})
	return nil
}

// GET /messages/{chat_jid}/{message_id}
func (s *server) getMessage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	m, err := s.db.message(ctx, r.PathValue("chat_jid"), r.PathValue("message_id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if m == nil {
		writeError(w, http.StatusNotFound, "no such message")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// GET /search?q=...
func (s *server) search(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	if err := s.searchErr(ctx, w, r); err != nil {
		s.handleErr(w, r, err)
	}
}

func (s *server) searchErr(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	text := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(text)) < 2 {
		return badRequest("q must be at least 2 characters")
	}
	limit, err := limitParam(r, defaultLimit, 200)
	if err != nil {
		return err
	}
	mq := messageQuery{limit: limit, orderBy: "m.sent_at DESC, m.rowid DESC", raw: boolParam(r, "raw")}
	if err := s.groupFilter(ctx, r, &mq); err != nil {
		return err
	}
	if err := s.timeFilter(r, &mq); err != nil {
		return err
	}
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)
	pattern := "%" + escaped + "%"
	mq.add(`(m.body LIKE ? ESCAPE '\' OR EXISTS (SELECT 1 FROM message_media_files f
		WHERE f.chat_jid = m.chat_jid AND f.message_id = m.message_id AND (f.caption LIKE ? ESCAPE '\' OR f.file_name LIKE ? ESCAPE '\')))`,
		pattern, pattern, pattern)
	msgs, err := s.db.messages(ctx, s.db.db, mq)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"query": text, "messages": msgs})
	return nil
}

// GET /media/{id}
func (s *server) media(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "id must be a file id, as in a message's files[].id")
		return
	}
	rel, mime, fileName, ok, err := s.db.mediaFile(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no such file, or its download failed (see the file's error)")
		return
	}
	path, ok := s.safePath(rel)
	if !ok {
		s.log.Printf("refused file %d: path %q is outside the media folder", id, rel)
		writeError(w, http.StatusNotFound, "no such file")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		writeError(w, http.StatusNotFound, "the file isn't in the media folder")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		writeError(w, http.StatusNotFound, "the file isn't in the media folder")
		return
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if fileName != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", strings.ReplaceAll(fileName, `"`, "")))
	}
	http.ServeContent(w, r, "", info.ModTime(), f)
}

// safePath resolves a relative_path under the media folder, refusing
// anything that would lead outside it.
func (s *server) safePath(rel string) (string, bool) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.Contains(rel, `\`) {
		return "", false
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return "", false
		}
	}
	root, err := filepath.Abs(s.mediaDir)
	if err != nil {
		return "", false
	}
	path := filepath.Join(root, filepath.FromSlash(rel))
	if inside, err := filepath.Rel(root, path); err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", false
	}
	// Follow symlinks too, so a link inside the folder can't point outside it.
	if real, err := filepath.EvalSymlinks(path); err == nil {
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return "", false
		}
		if inside, err := filepath.Rel(realRoot, real); err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return "", false
		}
	}
	return path, true
}

// GET /events — only for simulated databases.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := withTimeout(r)
	defer cancel()
	if !s.db.hasTable(ctx, "simulated_events") {
		writeError(w, http.StatusNotFound, "this isn't a simulated database, so there are no simulated events; /events only works with seth-sim data")
		return
	}
	limit, err := limitParam(r, 500, 5000)
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	since, _, err := intParam(r, "since")
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	events, err := s.db.simulatedEvents(ctx, since, r.URL.Query().Get("action"), limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

var _ querier = (*sql.Tx)(nil)

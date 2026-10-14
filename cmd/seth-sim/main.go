// Command seth-sim writes simulated Seth community WhatsApp traffic into a
// separate message database and media folder, so that downstream clients
// (AI agents and the like) can be built and tested without real messages.
//
// It writes through the same db package as seth-ingestion, so the schema and
// the write behaviour (albums, edits, failed downloads, gap filling) match the
// real database exactly.
//
//	seth-sim seed   [flags]   fill the database with a few days of history
//	seth-sim live   [flags]   keep adding messages, as if they were arriving now
//	seth-sim send   [flags]   add one message you describe
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tianqitong/seth-ingestion/db"
	"github.com/tianqitong/seth-ingestion/internal/grouplist"
)

const usage = `seth-sim writes simulated Seth WhatsApp group messages to a separate database,
for testing clients without real messages.

Usage:
  seth-sim seed [flags]   add a few days of history (source = 'history')
  seth-sim live [flags]   keep adding messages as they "arrive" (source = 'live'); Ctrl+C to stop
  seth-sim send [flags]   add one message you describe

Run "seth-sim <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		fmt.Print(usage)
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type common struct {
	dbPath, mediaDir, groupsFile, community string
	seed                                    int64
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.dbPath, "db", "data-sim/messages.db", "simulated message database to write (never the real one)")
	fs.StringVar(&c.mediaDir, "media-dir", "data-sim/media", "folder for simulated media files")
	fs.StringVar(&c.groupsFile, "groups-file", "groups.txt", "groups to simulate, one name per line")
	fs.StringVar(&c.community, "community", "Seth", "community name recorded with the groups")
	fs.Int64Var(&c.seed, "seed", 0, "random seed; the same seed gives the same messages (0 = random)")
}

func run(ctx context.Context, cmd string, args []string) error {
	fs := flag.NewFlagSet("seth-sim "+cmd, flag.ExitOnError)
	var c common
	c.register(fs)
	var (
		days     = fs.Int("days", 7, "seed: how many days of history to create, ending now")
		perDay   = fs.Int("per-day", 25, "seed: average messages per day across all groups")
		interval = fs.Duration("interval", 5*time.Second, "live: average time between messages")
		count    = fs.Int("count", 0, "live: stop after this many messages (0 = until Ctrl+C)")
		group    = fs.String("group", "Announcements", "send: group name, as in the groups file")
		from     = fs.String("from", "", "send: sender name (default: a random simulated parent)")
		kind     = fs.String("kind", "text", "send: text, reply, edit, image, album, document, voice, reaction or failed")
		text     = fs.String("text", "", "send: the message text or caption (default: a sample line)")
		target   = fs.String("target", "", "send: for reply, edit or reaction, the message_id to point at (default: a recent text in the group)")
	)
	switch cmd {
	case "seed", "live", "send":
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	fs.Parse(args)

	sim, err := open(ctx, c)
	if err != nil {
		return err
	}
	defer sim.close()

	switch cmd {
	case "seed":
		return sim.seed(ctx, *days, *perDay)
	case "live":
		return sim.live(ctx, *interval, *count)
	default:
		g := sim.groupNamed(*group)
		if g == nil {
			return fmt.Errorf("no group named %q in %s", *group, c.groupsFile)
		}
		k := kindByName(*kind)
		if k == nil {
			return fmt.Errorf("unknown kind %q", *kind)
		}
		return sim.emit(ctx, event{group: g, kind: k, at: time.Now(), source: "live", from: *from, text: *text, target: *target})
	}
}

// open prepares the simulated database: it refuses anything that looks like
// a real seth-ingestion database, then records the groups.
func open(ctx context.Context, c common) (*simulator, error) {
	if err := checkSafe(c.dbPath); err != nil {
		return nil, err
	}
	names, err := grouplist.ReadFile(c.groupsFile)
	if err != nil {
		return nil, fmt.Errorf("read groups file: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(c.dbPath), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(c.mediaDir, 0o755); err != nil {
		return nil, err
	}
	store, err := db.Open(ctx, c.dbPath)
	if err != nil {
		return nil, err
	}
	if err := markSimulated(ctx, c.dbPath, c.seed); err != nil {
		store.Close()
		return nil, fmt.Errorf("mark database as simulated: %w", err)
	}
	seed := c.seed
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	sim := newSimulator(store, c.dbPath, c.mediaDir, seed)
	if sim.write, err = sql.Open("sqlite3", "file:"+c.dbPath+"?_busy_timeout=5000"); err != nil {
		store.Close()
		return nil, err
	}
	communityJID := simJID("community " + c.community)
	for _, name := range names {
		isAnnouncement := grouplist.Key(name) == "announcements"
		jid := simJID(name)
		g, err := store.SaveGroup(ctx, db.Group{
			JID: jid, Name: name, CommunityJID: communityJID, CommunityName: c.community,
			IsAnnouncement: isAnnouncement, MediaFolder: grouplist.FolderName(name, jid[:len(jid)-len("@g.us")]),
		})
		if err != nil {
			store.Close()
			return nil, fmt.Errorf("record group %q: %w", name, err)
		}
		sim.groups = append(sim.groups, &g)
	}
	fmt.Printf("Simulating %d groups -> %s (media: %s)\n", len(sim.groups), c.dbPath, c.mediaDir)
	return sim, nil
}

// checkSafe refuses to write to a real seth-ingestion database: one next to a
// WhatsApp session, or one with messages but no simulation marker.
func checkSafe(dbPath string) error {
	if _, err := os.Stat(filepath.Join(filepath.Dir(dbPath), "whatsmeow-session.db")); err == nil {
		return fmt.Errorf("%s is next to a WhatsApp session, so it looks like the real database; choose another -db", dbPath)
	}
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	conn, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer conn.Close()
	var marked, hasMessages bool
	if err := conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'simulation')`).Scan(&marked); err != nil {
		return fmt.Errorf("check %s: %w", dbPath, err)
	}
	if marked {
		return nil
	}
	conn.QueryRow(`SELECT EXISTS (SELECT 1 FROM messages)`).Scan(&hasMessages)
	if hasMessages {
		return fmt.Errorf("%s has messages but wasn't made by seth-sim, so it may be real; choose another -db", dbPath)
	}
	return nil
}

// markSimulated adds a "simulation" table, so clients and seth-sim itself can
// tell this database apart from a real one.
func markSimulated(ctx context.Context, dbPath string, seed int64) error {
	conn, err := sql.Open("sqlite3", "file:"+dbPath+"?_busy_timeout=5000")
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS simulation (
			created_at TEXT NOT NULL,
			seed       INTEGER NOT NULL,
			note       TEXT NOT NULL
		);
		INSERT INTO simulation (created_at, seed, note)
		SELECT ?, ?, 'Made by seth-sim. Every message, person and phone number here is invented.'
		WHERE NOT EXISTS (SELECT 1 FROM simulation)`,
		time.Now().UTC().Format(time.RFC3339), seed)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, eventsSchema)
	return err
}

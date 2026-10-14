// Command seth-ingestion logs into WhatsApp as a linked device and records
// the messages and media of selected groups in a WhatsApp community into a
// local SQLite database and media folder.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/tianqitong/seth-ingestion/db"
	"github.com/tianqitong/seth-ingestion/internal/grouplist"
)

func main() {
	var (
		community  = flag.String("community", env("SETH_COMMUNITY", "Seth"), "name (or group JID) of the WhatsApp community whose groups to record")
		groupsFile = flag.String("groups-file", env("SETH_GROUPS_FILE", "groups.txt"), "file listing the community's groups to record, one name per line")
		dataDir    = flag.String("data-dir", env("SETH_DATA_DIR", "./data"), "directory for the session and message databases")
		mediaDir   = flag.String("media-dir", env("SETH_MEDIA_DIR", "/Users/tnpbot/Downloads/whatsapp-poc-images"), "directory to save images and other media into, in a folder per group")
		logLevel   = flag.String("log-level", env("SETH_LOG_LEVEL", "INFO"), "DEBUG, INFO, WARN or ERROR")
	)
	flag.BoolVar(&statusLines, "status-lines", env("SETH_STATUS_LINES", "") != "", "also print machine-readable \"@seth ...\" lines (the QR code and link status) for seth-dashboard")
	flag.Parse()

	if err := run(*community, *groupsFile, *dataDir, *mediaDir, *logLevel); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(community, groupsFile, dataDir, mediaDir, logLevel string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log := waLog.Stdout("Main", logLevel, true)
	groupNames, err := grouplist.ReadFile(groupsFile)
	if err != nil {
		return fmt.Errorf("read groups file: %w", err)
	}

	for _, dir := range []string{dataDir, mediaDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	container, err := sqlstore.New(ctx, "sqlite3",
		fmt.Sprintf("file:%s?_foreign_keys=on", filepath.Join(dataDir, "whatsmeow-session.db")),
		waLog.Stdout("Session", "WARN", true))
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	store, err := db.Open(ctx, filepath.Join(dataDir, "messages.db"))
	if err != nil {
		return fmt.Errorf("open message store: %w", err)
	}
	defer store.Close()
	if n, err := store.BackfillSender(ctx); err != nil {
		return fmt.Errorf("backfill sender: %w", err)
	} else if n > 0 {
		log.Infof("Set sender on %d existing messages", n)
	}
	if n, err := store.MigrateLegacyMedia(ctx, mediaDir); err != nil {
		return fmt.Errorf("move media to message_media_files: %w", err)
	} else if n > 0 {
		log.Infof("Moved %d existing media files into message_media_files", n)
	}

	cfg := sessionConfig{store: store, community: community, groupNames: groupNames, dataDir: dataDir, mediaDir: mediaDir, log: log}
	for {
		device, err := container.GetFirstDevice(ctx)
		if err != nil {
			return fmt.Errorf("load device: %w", err)
		}
		relink, err := runSession(ctx, device, cfg)
		if err != nil || !relink {
			return err
		}
		// whatsmeow has already deleted the revoked session, so the next
		// GetFirstDevice returns a fresh device and login shows a new QR code.
		log.Warnf("This device was unlinked from WhatsApp. Scan the new QR code to link it again, or press Ctrl+C to quit.")
		statusLine("status", "unlinked")
	}
}

type sessionConfig struct {
	store      *db.Store
	community  string
	groupNames []string
	dataDir    string
	mediaDir   string
	log        waLog.Logger
}

// runSession connects one linked device and ingests until shutdown or logout.
// It returns relink = true if WhatsApp logged the device out (for example,
// because it was removed under Linked Devices on the phone).
func runSession(ctx context.Context, device *store.Device, cfg sessionConfig) (relink bool, err error) {
	log := cfg.log
	client := whatsmeow.NewClient(device, waLog.Stdout("Client", "WARN", true))
	ingester := NewIngester(client, cfg.store, cfg.mediaDir, log)
	go ingester.Run(context.Background())
	defer ingester.Stop()
	client.AddEventHandler(ingester.HandleEvent)

	connected := make(chan struct{}, 1)
	loggedOut := make(chan struct{}, 1)
	replaced := make(chan struct{}, 1)
	client.AddEventHandler(func(evt any) {
		switch evt := evt.(type) {
		case *events.Connected:
			log.Infof("Connected as %s", client.Store.ID.User)
			statusLine("status", "connected")
			signalOnce(connected)
		case *events.LoggedOut:
			log.Warnf("Logged out by WhatsApp (reason: %s)", evt.Reason)
			signalOnce(loggedOut)
		case *events.StreamReplaced:
			signalOnce(replaced)
		}
	})

	if client.Store.ID == nil {
		if err := login(ctx, client, log); err != nil {
			return false, err
		}
	} else {
		log.Infof("Connecting as %s...", client.Store.ID.User)
		if err := client.Connect(); err != nil {
			return false, fmt.Errorf("connect: %w", err)
		}
	}
	defer client.Disconnect()

	// The groups can only be looked up once WhatsApp has accepted the session.
	// Until then, messages wait in the ingester's queue.
	for found := false; ; {
		select {
		case <-ctx.Done():
			log.Infof("Shutting down")
			return false, nil
		case <-loggedOut:
			return true, nil
		case <-replaced:
			return false, fmt.Errorf("another process connected with this session; only run one instance at a time")
		case <-connected:
		}
		if found {
			continue // reconnected after a network drop; the groups are already known
		}
		groups, err := findGroups(ctx, client, cfg.store, cfg.community, cfg.groupNames, log)
		if ctx.Err() != nil {
			continue // Ctrl+C while looking up the groups
		}
		if err != nil {
			return false, err
		}
		found = true
		// Read the names before SetGroups: after it, only the worker may use the groups.
		names := make([]string, 0, len(groups))
		for _, g := range groups {
			names = append(names, g.Name)
		}
		sort.Strings(names)
		ingester.SetGroups(groups)
		log.Infof("Recording %d groups: %s", len(names), strings.Join(names, ", "))
		statusLine("status", "recording", strconv.Itoa(len(names)))
		log.Infof("Messages -> %s, media -> %s. Press Ctrl+C to stop.", filepath.Join(cfg.dataDir, "messages.db"), cfg.mediaDir)
	}
}

func signalOnce(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// login links this program as a new device by showing a QR code in the terminal.
func login(ctx context.Context, client *whatsmeow.Client, log waLog.Logger) error {
	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("get QR channel: %w", err)
	}
	if err := client.Connect(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	fmt.Println("\nOn your phone: WhatsApp → Settings → Linked Devices → Link a Device, then scan:")
	for item := range qrChan {
		switch item.Event {
		case whatsmeow.QRChannelEventCode:
			qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stdout)
			statusLine("qr", item.Code)
			fmt.Printf("(QR refreshes in %s)\n", item.Timeout)
		case whatsmeow.QRChannelSuccess.Event:
			log.Infof("Login successful")
			statusLine("status", "linked")
			return nil
		case whatsmeow.QRChannelTimeout.Event:
			statusLine("status", "qr_timeout")
			return fmt.Errorf("QR code timed out; run again to get a new one")
		case whatsmeow.QRChannelEventError:
			return fmt.Errorf("pairing failed: %w", item.Error)
		default:
			log.Infof("Login event: %s", item.Event)
		}
	}
	if ctx.Err() != nil {
		return nil // Ctrl+C while waiting for the scan
	}
	return fmt.Errorf("login aborted")
}

// statusLines turns on statusLine. seth-dashboard sets it, to show the QR code
// and the link status in the browser.
var statusLines bool

// statusLine prints a line such as "@seth qr <code>" or "@seth status
// connected" on stdout, for seth-dashboard to read. It prints nothing unless
// -status-lines is set. The words after "@seth" are:
//
//	qr <code>               a new QR code to scan (replaces the previous one)
//	status linked           the QR code was scanned and the device is linked
//	status qr_timeout       no one scanned the QR code in time; the program exits
//	status connected        connected to WhatsApp
//	status recording <n>    found the groups and is recording n of them
//	status unlinked         the device was unlinked on the phone; a new QR code follows
func statusLine(words ...string) {
	if statusLines {
		fmt.Println("@seth " + strings.Join(words, " "))
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

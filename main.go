// Command seth-ingestion logs into WhatsApp as a linked device and records
// every message and image sent by one contact into a local SQLite database
// and image folder.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/tianqitong/seth-ingestion/db"
)

func main() {
	var (
		contactPhone = flag.String("contact-phone", env("SETH_CONTACT_PHONE", "14125778174"), "phone number of the contact to monitor, with country code")
		contactName  = flag.String("contact-name", env("SETH_CONTACT_NAME", "Ziwei Hu"), "name of the contact, stored in the sender column")
		dataDir      = flag.String("data-dir", env("SETH_DATA_DIR", "./data"), "directory for the session and message databases")
		mediaDir     = flag.String("media-dir", env("SETH_MEDIA_DIR", "/Users/tnpbot/Downloads/whatsapp-poc-images"), "directory to save images and other media into")
		logLevel     = flag.String("log-level", env("SETH_LOG_LEVEL", "INFO"), "DEBUG, INFO, WARN or ERROR")
	)
	flag.Parse()

	if err := run(*contactPhone, *contactName, *dataDir, *mediaDir, *logLevel); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(contactPhone, contactName, dataDir, mediaDir, logLevel string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log := waLog.Stdout("Main", logLevel, true)
	phone := digitsOnly(contactPhone)
	if len(phone) == 10 {
		phone = "1" + phone // assume a US number when no country code is given
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
	if n, err := store.BackfillSender(ctx, phone, contactName); err != nil {
		return fmt.Errorf("backfill sender: %w", err)
	} else if n > 0 {
		log.Infof("Set sender = %q on %d existing messages", contactName, n)
	}
	if n, err := store.MigrateLegacyMedia(ctx, mediaDir); err != nil {
		return fmt.Errorf("move media to message_media_files: %w", err)
	} else if n > 0 {
		log.Infof("Moved %d existing media files into message_media_files", n)
	}

	cfg := sessionConfig{store: store, phone: phone, contactName: contactName, dataDir: dataDir, mediaDir: mediaDir, log: log}
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
	}
}

type sessionConfig struct {
	store       *db.Store
	phone       string
	contactName string
	dataDir     string
	mediaDir    string
	log         waLog.Logger
}

// runSession connects one linked device and ingests until shutdown or logout.
// It returns relink = true if WhatsApp logged the device out (for example,
// because it was removed under Linked Devices on the phone).
func runSession(ctx context.Context, device *store.Device, cfg sessionConfig) (relink bool, err error) {
	log := cfg.log
	client := whatsmeow.NewClient(device, waLog.Stdout("Client", "WARN", true))
	ingester := NewIngester(client, cfg.store, cfg.phone, cfg.contactName, cfg.mediaDir, log)
	go ingester.Run(context.Background())
	defer ingester.Stop()
	client.AddEventHandler(ingester.HandleEvent)

	loggedOut := make(chan struct{}, 1)
	replaced := make(chan struct{}, 1)
	client.AddEventHandler(func(evt any) {
		switch evt := evt.(type) {
		case *events.Connected:
			log.Infof("Connected as %s. Monitoring direct messages from %s (+%s)", client.Store.ID.User, cfg.contactName, cfg.phone)
			log.Infof("Messages -> %s, media -> %s. Press Ctrl+C to stop.", filepath.Join(cfg.dataDir, "messages.db"), cfg.mediaDir)
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

	select {
	case <-ctx.Done():
		log.Infof("Shutting down")
		return false, nil
	case <-loggedOut:
		return true, nil
	case <-replaced:
		return false, fmt.Errorf("another process connected with this session; only run one instance at a time")
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
			fmt.Printf("(QR refreshes in %s)\n", item.Timeout)
		case whatsmeow.QRChannelSuccess.Event:
			log.Infof("Login successful")
			return nil
		case whatsmeow.QRChannelTimeout.Event:
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

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func digitsOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

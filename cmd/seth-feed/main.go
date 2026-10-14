// Command seth-feed serves the messages that seth-ingestion records as a
// read-only REST API with JSON responses, so that AI agents and other
// programs can follow and read them over HTTP instead of opening the SQLite
// database themselves.
//
// It opens the database read-only and never changes it. Point it at the
// simulator's data with -sim to test clients without real messages.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var (
		dbPath   = flag.String("db", env("SETH_FEED_DB", "data/messages.db"), "message database to serve (opened read-only)")
		mediaDir = flag.String("media-dir", env("SETH_MEDIA_DIR", "/Users/tnpbot/Downloads/whatsapp-poc-images"), "media folder the database's relative_path values point into")
		addr     = flag.String("addr", env("SETH_FEED_ADDR", "127.0.0.1:8080"), "address to listen on; 127.0.0.1 keeps it to this computer, 0.0.0.0 opens it to the network")
		sim      = flag.Bool("sim", false, "serve the simulator's data (data-sim/messages.db and data-sim/media) instead")
		cors     = flag.String("cors-origin", env("SETH_FEED_CORS_ORIGIN", ""), "allow browser pages from this origin (or *) to call the API; empty sends no CORS headers")
	)
	flag.Parse()
	if *sim {
		*dbPath, *mediaDir = "data-sim/messages.db", "data-sim/media"
	}
	logger := log.New(os.Stderr, "seth-feed ", log.LstdFlags)
	if err := run(*dbPath, *mediaDir, *addr, *cors, logger); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(dbPath, mediaDir, addr, corsOrigin string, logger *log.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	db, err := openReader(dbPath)
	if err != nil {
		return err
	}
	defer db.close()
	s := &server{db: db, dbPath: dbPath, mediaDir: mediaDir, corsOrigin: corsOrigin, log: logger}

	kind := "real"
	if db.hasTable(ctx, "simulation") {
		kind = "SIMULATED"
	}
	logger.Printf("Serving %s data from %s (read-only), media from %s", kind, dbPath, mediaDir)
	logger.Printf("Listening on http://%s. Press Ctrl+C to stop.", addr)

	srv := &http.Server{
		Addr:              addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      2 * time.Minute, // room for large media files
		IdleTimeout:       2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		logger.Printf("Shutting down")
		shutdown, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

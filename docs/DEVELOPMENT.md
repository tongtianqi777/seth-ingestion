# Development guide

This guide covers how seth-ingestion is built and how to change it. For setup and usage, see the [README](../README.md). For the tables, how messages become rows, and schema upgrades, see [DATABASE.md](DATABASE.md).

## Layout

```
main.go          flags/env config, opening the stores, QR login, signal handling
ingest.go        event handling: filtering by contact, parsing messages, downloading media
db/schema.sql    message table DDL (embedded into the binary and applied on startup)
db/db.go         SQLite access for the message store, plus migrations from older schemas
db/db_test.go    tests for album grouping and the legacy media migration
run.sh           entry script: rebuilds when sources change, then runs bin/seth-ingestion
docs/            this guide and DATABASE.md (schema and storage logic)
data/            runtime state (git-ignored)
  whatsmeow-session.db   whatsmeow's device keys and session, managed by whatsmeow
  messages.db            our ingested messages
```

## How it works

```mermaid
flowchart TD
    WA["WhatsApp"] -- websocket --> Client["whatsmeow.Client"]
    Client -- "events.Message (live)<br/>events.HistorySync (once, after linking)" --> Handle["Ingester.HandleEvent<br/>(runs on whatsmeow's event goroutine; must stay fast)"]
    Handle -- "from me, not a direct chat<br/>(group, status, broadcast, newsletter),<br/>or not from the target contact" --> Drop(["dropped"])
    Handle -- "from the target contact" --> Queue[["buffered queue (1024)"]]
    Queue --> Run["Ingester.Run (single worker)"]
    Run --> Classify{"classify"}
    Classify -- "text / edit / album / other" --> Save[("db.Store.Save<br/>messages + message_media_files<br/>(one transaction)")]
    Classify -- "has media" --> Seen{"file already<br/>stored?"}
    Seen -- yes --> Skip(["skipped"])
    Seen -- no --> Download["download and decrypt"]
    Download --> Media[("media dir")]
    Download -- "media file: relative_path,<br/>size, sha256 (or error)" --> Save
```

- **Why a worker queue?** whatsmeow dispatches events one at a time. A slow media download inside the handler would hold up all other events, including the session's own housekeeping. The single worker also keeps rows in arrival order.
- **Idempotency.** `messages` is keyed on `(chat_jid, message_id)` and `message_media_files` is unique on `(chat_jid, source_message_id)`. A repeated message never creates a second row. Before downloading, the worker checks `MediaFileExists`, so a history sync that repeats messages doesn't fetch their files again. The full write rules are in [DATABASE.md](DATABASE.md#write-rules).
- **Sessions and logout.** `run` loops over `runSession`. Each session gets its own `whatsmeow.Client` and `Ingester`. When WhatsApp sends `events.LoggedOut` (the device was removed on the phone, or the session was revoked), whatsmeow deletes the device from the session store. `runSession` then returns `relink = true`. The loop calls `GetFirstDevice` again, gets a fresh device, and `login` shows a new QR code. `events.StreamReplaced` (a second instance using the same session) ends the program with an error. The "Monitoring…" log line is written on `events.Connected`, so it only appears once the session is accepted.
- **Shutdown.** On Ctrl+C or SIGTERM, the client disconnects first, then `Ingester.Stop` closes the queue and waits for the worker to finish what's left. Deferred calls run in reverse order: `Disconnect`, then `Stop`, then `store.Close`.

### Contact matching (phone numbers vs LIDs)

WhatsApp is moving users from phone-number JIDs (`14125778174@s.whatsapp.net`) to opaque *LIDs* (`123456789@lid`). A message's sender can come in either form. `Ingester.targetSender` checks both `Info.Sender` and `Info.SenderAlt`, and resolves LIDs to phone numbers with `client.Store.LIDs.GetPNForLID`. whatsmeow fills that mapping as it sees traffic and history. If a message from the contact is missed, a missing LID mapping is the first thing to check.

`sender_jid` stores whatever WhatsApp sent, which may be a LID. `sender_phone` stores the resolved phone number.

### Direct chats only

`isDirectChat` accepts a chat only if its JID is on the user server (`@s.whatsapp.net`) or the LID server (`@lid`). That excludes groups (`@g.us`), status updates and broadcast lists (`@broadcast`), and newsletters. `enqueue` also checks `Info.IsGroup`, and history sync skips whole non-direct conversations before parsing them.

### The `sender` column

`sender` holds the monitored contact's configured name (`-contact-name`); see [DATABASE.md](DATABASE.md#messages). To monitor more than one contact later, replace `targetPhone`/`targetName` with a phone→name map and have `targetSender` return the matching name.

### Message classification

`Ingester.process` decides each message's `message_type` and its media files. The mapping, including how albums are stored as one message, is in [DATABASE.md](DATABASE.md#message-types). Two details matter in the code:

- whatsmeow unwraps ephemeral, view-once and similar wrappers before the event arrives, so `evt.Message` is the inner message. The exception is `AssociatedChildMessage`, which wraps album items; `process` unwraps that itself.
- `mediaOf` lists the media types that are downloaded, and `albumParent` detects album items. To support another media type, add a case to `mediaOf`. Any message type with media implements `whatsmeow.DownloadableMessage`.

## Database

Schema, write rules and upgrades are documented in [DATABASE.md](DATABASE.md). When you change the schema, update `db/schema.sql`, the migration steps in `db/db.go`, and DATABASE.md together.

## Building and checking

```bash
go build -o bin/seth-ingestion .   # what run.sh does (needs CGO for mattn/go-sqlite3)
go test ./...
go vet ./...
gofmt -l .                          # should print nothing
```

`db/db_test.go` covers how albums are stored as one message (including items arriving before the album message, and duplicates) and the legacy media migration. `Ingester.process` isn't tested yet. The next step would be to give it an `events.Message` built from a `waE2E.Message` literal, run it against a temp SQLite store, and use a fake downloader.

To test by hand, link a device with a separate `-data-dir` so the real session is left alone. Then have the contact (or yourself, temporarily using `-contact-phone` with a second account's number) send a text, an image, an album of 2–3 images, a voice note, a document, a reply, and an edit:

```bash
./run.sh -data-dir ./data-dev -media-dir /tmp/seth-images -log-level DEBUG
```

## Dependencies

| Module | Purpose |
|---|---|
| `go.mau.fi/whatsmeow` | WhatsApp multi-device protocol client |
| `github.com/mattn/go-sqlite3` | SQLite driver, for both the whatsmeow session store and our messages |
| `github.com/mdp/qrterminal/v3` | renders the login QR code in the terminal |

whatsmeow has no tagged releases, so `go.mod` pins a pseudo-version. WhatsApp changes its protocol now and then. If login or decryption starts failing, update first:

```bash
go get go.mau.fi/whatsmeow@latest && go mod tidy && go build ./...
```

## Caveats

- This is an unofficial client. WhatsApp may rate-limit or ban accounts that behave unusually. Passive reading like this is low risk, but avoid running several linked instances at once.
- A linked device is logged out if the phone doesn't come online for about 14 days.
- History sync is best-effort. How much history arrives, and whether its media can still be downloaded, is up to WhatsApp.

# Development guide

This guide covers how seth-ingestion is built and how to change it. For setup and usage, see the [README](../README.md).

## Layout

```
main.go          flags/env config, opening the stores, QR login, signal handling
ingest.go        event handling: filtering by contact, parsing messages, downloading images
db/schema.sql    message table DDL (embedded into the binary and applied on startup)
db/db.go         SQLite access for the message store
run.sh           entry script: rebuilds when sources change, then runs bin/seth-ingestion
data/            runtime state (git-ignored)
  whatsmeow-session.db   whatsmeow's device keys and session, managed by whatsmeow
  messages.db            our ingested messages
```

## How it works

```
WhatsApp ──websocket──▶ whatsmeow.Client
                           │  events.Message      (live)
                           │  events.HistorySync  (sent once after linking)
                           ▼
                    Ingester.HandleEvent   ── runs on whatsmeow's event goroutine and must stay fast
                           │  drops messages that are from me, not in a direct chat
                           │  (groups, status, broadcast lists, newsletters), or not from the target contact
                           ▼
                    buffered queue (1024)
                           ▼
                    Ingester.Run (single worker)
                           │  skips messages already stored
                           │  classifies the message: text / image / edit / other
                           │  downloads and decrypts images, then writes them to the image dir
                           ▼
                    db.Store.Insert  (ON CONFLICT DO NOTHING)
```

- **Why a worker queue?** whatsmeow dispatches events one at a time. A slow image download inside the handler would hold up all other events, including the session's own housekeeping. The single worker also keeps rows in arrival order.
- **Idempotency.** `(chat_jid, message_id)` is the primary key. The worker checks `Exists` before downloading, so a history sync that repeats messages doesn't fetch their images again.
- **Sessions and logout.** `run` loops over `runSession`. Each session gets its own `whatsmeow.Client` and `Ingester`. When WhatsApp sends `events.LoggedOut` (the device was removed on the phone, or the session was revoked), whatsmeow deletes the device from the session store. `runSession` then returns `relink = true`. The loop calls `GetFirstDevice` again, gets a fresh device, and `login` shows a new QR code. `events.StreamReplaced` (a second instance using the same session) ends the program with an error. The "Monitoring…" log line is written on `events.Connected`, so it only appears once the session is accepted.
- **Shutdown.** On Ctrl+C or SIGTERM, the client disconnects first, then `Ingester.Stop` closes the queue and waits for the worker to finish what's left. Deferred calls run in reverse order: `Disconnect`, then `Stop`, then `store.Close`.

### Contact matching (phone numbers vs LIDs)

WhatsApp is moving users from phone-number JIDs (`14125778174@s.whatsapp.net`) to opaque *LIDs* (`123456789@lid`). A message's sender can come in either form. `Ingester.targetSender` checks both `Info.Sender` and `Info.SenderAlt`, and resolves LIDs to phone numbers with `client.Store.LIDs.GetPNForLID`. whatsmeow fills that mapping as it sees traffic and history. If a message from the contact is missed, a missing LID mapping is the first thing to check.

`sender_jid` stores whatever WhatsApp sent, which may be a LID. `sender_phone` stores the resolved phone number.

### Direct chats only

`isDirectChat` accepts a chat only if its JID is on the user server (`@s.whatsapp.net`) or the LID server (`@lid`). That excludes groups (`@g.us`), status updates and broadcast lists (`@broadcast`), and newsletters. `enqueue` also checks `Info.IsGroup`, and history sync skips whole non-direct conversations before parsing them.

### The `sender` column

`sender` holds the monitored contact's configured name (`-contact-name`), not anything WhatsApp sends. It's a stable label for grouping rows by contact, and it's ready for monitoring more than one contact later. To do that, replace `targetPhone`/`targetName` with a phone→name map and have `targetSender` return the matching name. `sender_name` is different: it's the push name the contact has set for themselves in WhatsApp, and it can change over time.

### Message classification

The logic is in `Ingester.process`. whatsmeow has already unwrapped ephemeral, view-once and similar wrappers, so `evt.Message` is the inner message.

| Condition | `message_type` |
|---|---|
| `ProtocolMessage` of type `MESSAGE_EDIT` | `edit` (`body` holds the new text, `edit_target_id` the original ID) |
| `ImageMessage` | `image` |
| `Conversation` or `ExtendedTextMessage` | `text` |
| anything else | `other` |

Every row also stores the full message as protobuf JSON in `raw_json`. New types can then be supported by backfilling from the database, with no need to receive the messages again.

To capture another media type, such as video, documents or voice notes, add a case that calls `client.Download` on that message type, as `saveImage` does. Any message type with media implements `whatsmeow.DownloadableMessage`.

## Database

`db/schema.sql` is embedded with `//go:embed` and run on every startup. It uses only `CREATE … IF NOT EXISTS`, so it is safe to re-run. It always describes the full current schema, so fresh databases come straight from it. Databases created by older versions are brought up to date by `migrate` in `db/db.go`, which runs before `schema.sql`. That ordering is needed because `schema.sql`'s indexes may reference new columns. To change the schema:

- **Adding a column:** add it to the `CREATE TABLE` in `schema.sql`, and add an `if !cols["…"] { ALTER TABLE messages ADD COLUMN … }` step to `migrate`. `sender` is the example to follow. SQLite only allows `NOT NULL` on an added column if it has a default. If existing rows need a real value, backfill them after opening, the way `BackfillSender` is called from `main.go`.
- **Anything larger:** introduce `PRAGMA user_version` and versioned migration steps in `db.Open`.

The session database belongs to whatsmeow, and `sqlstore` upgrades it on its own. Don't write to it.

Timestamps are stored as RFC3339 UTC strings, so they sort correctly as text.

## Building and checking

```bash
go build -o bin/seth-ingestion .   # what run.sh does (needs CGO for mattn/go-sqlite3)
go vet ./...
gofmt -l .                          # should print nothing
```

There are no automated tests yet. The parts worth testing first are `extFor`, `digitsOnly`, and the classification in `process`. To test classification, give `process` an `events.Message` built from a `waE2E.Message` literal, run it against a temp SQLite store, and use a fake downloader.

To test by hand, link a device with a separate `-data-dir` so the real session is left alone. Then have the contact (or yourself, temporarily using `-contact-phone` with a second account's number) send a text, an image, a reply, and an edit:

```bash
./run.sh -data-dir ./data-dev -image-dir /tmp/seth-images -log-level DEBUG
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

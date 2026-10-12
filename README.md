# seth-ingestion

Links to your WhatsApp account as a device and records every message and media file that **Ziwei Hu (+1 412-577-8174)** sends you:

- **Messages** go to a local SQLite database: `./data/messages.db`
- **Images and other media** (videos, voice notes, documents, stickers) go to `/Users/tnpbot/Downloads/whatsapp-poc-images`

It's built on [whatsmeow](https://github.com/tulir/whatsmeow) and written in Go.

## Prerequisites

- macOS with the Xcode command line tools (`xcode-select --install`). The SQLite driver needs a C compiler.
- Go 1.25 or newer (`brew install go`)
- A phone with WhatsApp signed in

You don't need a database server. SQLite is a single file, and the program creates the file and its tables on first run.

## Start

```bash
./run.sh
```

The first run builds the program, then prints a QR code in the terminal:

1. On your phone, open **WhatsApp → Settings → Linked Devices → Link a Device**.
2. Scan the QR code. The code refreshes every ~20 seconds. If it times out, run `./run.sh` again.
3. Once you see `Logged in as …`, the program is monitoring.

Later runs reuse the saved session in `./data/whatsmeow-session.db`, so you won't need to scan again. Press **Ctrl+C** to stop.

Just after you link the device, WhatsApp also sends some recent chat history. The program ingests any of the contact's messages it finds there too. These are recorded with `source = 'history'`, and messages that arrive while the program is running are recorded as `'live'`.

## What gets captured

The program records every message the contact sends you in your **direct (one-to-one) chat**. Messages they send in groups are ignored, and so are status updates and broadcast lists. There are two tables:

- **`messages`** has exactly one row per message, with the contact's name (`Ziwei Hu`) in `sender`.
- **`message_media_files`** has one row per media file. Each row points to its message through `chat_jid` + `message_id`, and `relative_path` gives the file's location within the media folder.

| Kind | In `messages` | In `message_media_files` |
|---|---|---|
| Text (including replies) | a `text` row with the text in `body` | none |
| Image, video, voice note, audio, document or sticker (including view-once) | one row of that type (`image`, `video`, `voice`, …). Any caption goes in `body` | one row for the file |
| Several images or videos sent together (an album) | **one** `album` row | one row per file, in album order (`position`) |
| Edit to an earlier message | an `edit` row with the new text, and `edit_target_id` pointing to the original | none |
| Anything else (reactions, locations…) | an `other` row. The full content is kept in `raw_json` | none |

Files are named `<UTC time>_<message id>.<ext>`. `messages.media_file_count` gives the number of files for each message. Your own messages are not recorded.

## Querying the data

Recent messages:

```bash
sqlite3 -header -column data/messages.db \
  "SELECT sent_at, sender, message_type, body, media_file_count FROM messages ORDER BY sent_at DESC LIMIT 20;"
```

Messages with their files:

```bash
sqlite3 -header -column data/messages.db \
  "SELECT m.sent_at, m.message_type, a.position, a.media_type, a.relative_path
   FROM messages m JOIN message_media_files a USING (chat_jid, message_id)
   ORDER BY m.sent_at DESC, a.position;"
```

The schema, with each column explained, is in [db/schema.sql](db/schema.sql).

## Configuration

The defaults match the setup above. To change them, use flags (`./run.sh -h`) or environment variables:

| Flag | Env var | Default |
|---|---|---|
| `-contact-phone` | `SETH_CONTACT_PHONE` | `14125778174` |
| `-contact-name` | `SETH_CONTACT_NAME` | `Ziwei Hu` (stored in the `sender` column) |
| `-media-dir` | `SETH_MEDIA_DIR` | `/Users/tnpbot/Downloads/whatsapp-poc-images` (all media goes here) |
| `-data-dir` | `SETH_DATA_DIR` | `./data` |
| `-log-level` | `SETH_LOG_LEVEL` | `INFO` |

Phone numbers can include formatting, as in `-contact-phone "+1 (412) 577-8174"`. A 10-digit number is treated as a US number.

## Logging out / starting over

- If you remove the device under **Linked Devices** on your phone while the program is running, it logs `This device was unlinked from WhatsApp` and shows a new QR code. Scan it to link again, or press Ctrl+C to quit. The old session is deleted automatically, so the next `./run.sh` also starts with a QR code.
- To clear the captured data, delete `data/messages.db` and the media folder.

## Troubleshooting

- **The contact's messages aren't being captured.** Newer WhatsApp chats can identify people by a hidden ID instead of their phone number. The program translates these IDs back to phone numbers. For an unfamiliar contact, though, it may need a few minutes after linking to learn the mapping. Run with `-log-level DEBUG` to see what arrives.
- **`Failed to download …`.** WhatsApp only keeps media on its servers for a limited time. Files in older history may have expired. The message and media file rows are still saved, with the reason in the media file row's `error` column.

Developer documentation is in [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md).

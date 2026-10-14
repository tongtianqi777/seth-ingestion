# seth-ingestion

Links to your WhatsApp account as a device and records the messages and media files in the groups of the **Seth** community that are listed in [`groups.txt`](groups.txt):

- **Messages** go to a local SQLite database: `./data/messages.db`. Each one is recorded with the group it was sent in.
- **Images and other media** (videos, voice notes, documents, stickers) go to `/Users/tnpbot/Downloads/whatsapp-poc-images`, in a folder per group, such as `preschool/`.

It's built on [whatsmeow](https://github.com/tulir/whatsmeow) and written in Go.

## Prerequisites

- macOS with the Xcode command line tools (`xcode-select --install`). The SQLite driver needs a C compiler.
- Go 1.26.1 or newer (`brew install go`)
- A phone with WhatsApp signed in, as a member of the community

You don't need a database server. SQLite is a single file, and the program creates the file and its tables on first run.

## Start

```bash
./run.sh
```

The first run builds the program, then prints a QR code in the terminal:

1. On your phone, open **WhatsApp → Settings → Linked Devices → Link a Device**.
2. Scan the QR code. The code refreshes every ~20 seconds. If it times out, run `./run.sh` again.
3. Once you see `Recording N groups: …`, the program is monitoring.

Later runs reuse the saved session in `./data/whatsmeow-session.db`, so you won't need to scan again. Press **Ctrl+C** to stop.

Just after you link the device, WhatsApp also sends some recent chat history. The program records the listed groups' messages it finds there too. These are recorded with `source = 'history'`, and messages that arrive while the program is running are recorded as `'live'`.

## Choosing the groups

[`groups.txt`](groups.txt) lists the groups to record, one per line, with their names as WhatsApp shows them. Case and extra spaces don't matter, and lines starting with `#` are ignored. `Announcements` means the community's announcement group. Restart the program after editing the file.

Only groups that are both listed and part of the Seth community are recorded. Messages in any other group, in direct chats, in status updates and in broadcast lists are ignored. When it starts, the program logs:

- `Recording N groups: …`, the groups it found and is recording.
- A warning for each listed group it couldn't find in the community, for example because it was renamed. Fix the name in `groups.txt`.
- `Not recording these groups in the Seth community …`, the community's other groups you're in. Add any you want to `groups.txt`.

## What gets captured

The program records every message in the listed groups. Your own messages are recorded the same way as everyone else's. There are three tables:

- **`chat_groups`** has one row per recorded group: its name, its community, and its folder in the media folder.
- **`messages`** has exactly one row per message. `chat_jid` is the group's ID, which joins to `chat_groups.group_jid`. `sender` names the sender as WhatsApp would: your saved contact name for them, otherwise the name they set for themselves, otherwise their phone number.
- **`message_media_files`** has one row per media file. Each row points to its message through `chat_jid` + `message_id`, and `relative_path` gives the file's location within the media folder.

| Kind | In `messages` | In `message_media_files` |
|---|---|---|
| Text (including replies) | a `text` row with the text in `body` | none |
| Image, video, voice note, audio, document or sticker (including view-once) | one row of that type (`image`, `video`, `voice`, …). Any caption goes in `body` | one row for the file |
| Several images or videos sent together (an album) | **one** `album` row | one row per file, in album order (`position`) |
| Edit to an earlier message | an `edit` row with the new text, and `edit_target_id` pointing to the original | none |
| Anything else (reactions, locations, polls…) | an `other` row. The full content is kept in `raw_json` | none |

Each group's files are in its own folder within the media folder, named `<UTC time>_<message id>.<ext>`. The folder name is the group's name in lowercase, with dashes instead of spaces and punctuation, for example `preschool/`, `cleanup-duties-campus-security/` or `preschool-pe-teachers/`. It's set when the group is first recorded, and kept if the group is renamed later. `chat_groups.media_folder` records each group's folder, and `messages.media_file_count` gives the number of files for each message.

Notices such as "… joined from the community" aren't recorded.

## Querying the data

Recent messages, with their groups:

```bash
sqlite3 -header -column data/messages.db \
  "SELECT m.sent_at, g.name AS grp, m.sender, m.message_type, m.body
   FROM messages m JOIN chat_groups g ON g.group_jid = m.chat_jid
   ORDER BY m.sent_at DESC LIMIT 20;"
```

One group's files:

```bash
sqlite3 -header -column data/messages.db \
  "SELECT m.sent_at, m.sender, f.media_type, f.relative_path
   FROM messages m
   JOIN chat_groups g ON g.group_jid = m.chat_jid
   JOIN message_media_files f USING (chat_jid, message_id)
   WHERE g.name = 'Preschool'
   ORDER BY m.sent_at DESC, f.position;"
```

[docs/DATABASE.md](docs/DATABASE.md) explains every table and column, how albums and edits are stored, and has more example queries.

## Configuration

The defaults match the setup above. To change them, use flags (`./run.sh -h`) or environment variables:

| Flag | Env var | Default |
|---|---|---|
| `-community` | `SETH_COMMUNITY` | `Seth`: the community's name, or its group JID if several communities share the name |
| `-groups-file` | `SETH_GROUPS_FILE` | `groups.txt` |
| `-media-dir` | `SETH_MEDIA_DIR` | `/Users/tnpbot/Downloads/whatsapp-poc-images` (a folder per group goes here) |
| `-data-dir` | `SETH_DATA_DIR` | `./data` |
| `-log-level` | `SETH_LOG_LEVEL` | `INFO` |

## Logging out / starting over

- If you remove the device under **Linked Devices** on your phone while the program is running, it logs `This device was unlinked from WhatsApp` and shows a new QR code. Scan it to link again, or press Ctrl+C to quit. The old session is deleted automatically, so the next `./run.sh` also starts with a QR code.
- To clear the captured data, delete `data/messages.db` and the media folder.

## Earlier versions

Earlier versions recorded one contact's direct messages instead of groups. Upgrading keeps those rows (`is_group = 0`) and their files at the top of the media folder. They have no `chat_groups` row.

## Troubleshooting

- **A group isn't being recorded.** Check the startup log. A listed group that wasn't found gets a warning: compare its name in `groups.txt` with WhatsApp. Groups are matched by name when the program starts, so after a group is renamed, update `groups.txt` before the next start. (A rename while the program is running is picked up, and the group stays in the same media folder.)
- **`you're not in a community named "Seth"`.** The error lists the communities that were found. Pass the right name with `-community`.
- **`Failed to download …`.** WhatsApp only keeps media on its servers for a limited time. Files in older history may have expired. The message and media file rows are still saved, with the reason in the media file row's `error` column.
- **A sender has no phone number.** Newer WhatsApp groups can identify people by a hidden ID instead of their phone number. The program translates these IDs back to phone numbers where WhatsApp has shared the mapping, and stores the hidden ID in `sender_jid` either way. Run with `-log-level DEBUG` to see what arrives.

Developer documentation is in [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md), and the database is documented in [docs/DATABASE.md](docs/DATABASE.md).

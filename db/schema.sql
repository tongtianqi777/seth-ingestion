-- seth-ingestion message store (SQLite).
-- Applied automatically on startup; safe to run repeatedly.
-- Databases created by older versions are upgraded by db.Open and Store.MigrateLegacyMedia (see db.go).

-- One row per WhatsApp message. A message with several files (an album) is still one row;
-- its files are rows in message_media_files.
CREATE TABLE IF NOT EXISTS messages (
    -- Identity. WhatsApp message IDs are unique per chat, so (chat_jid, message_id) is the key.
    chat_jid        TEXT    NOT NULL,              -- the contact's JID (only direct chats are ingested)
    message_id      TEXT    NOT NULL,

    -- Sender
    sender          TEXT    NOT NULL DEFAULT '',   -- name of the monitored contact as configured (-contact-name), e.g. Ziwei Hu
    sender_jid      TEXT    NOT NULL,              -- may be a phone JID (@s.whatsapp.net) or a LID (@lid)
    sender_phone    TEXT,                          -- resolved E.164 digits, e.g. 14125778174
    sender_name     TEXT,                          -- the contact's own WhatsApp display name at the time of the message
    is_group        INTEGER NOT NULL DEFAULT 0,    -- always 0 now; 1 only on rows captured before group messages were excluded

    -- Timing
    sent_at         TEXT    NOT NULL,              -- RFC3339 UTC, from WhatsApp
    ingested_at     TEXT    NOT NULL,              -- RFC3339 UTC, when this row was written

    -- Content
    message_type    TEXT    NOT NULL,              -- text | image | video | audio | voice | document | sticker | album | edit | other
    body            TEXT,                          -- text body, or the media caption (for an album, the first caption received)
    edit_target_id  TEXT,                          -- for message_type = 'edit': the edited message's ID
    quoted_id       TEXT,                          -- ID of the message this one replies to, if any
    media_file_count INTEGER NOT NULL DEFAULT 0,   -- number of rows in message_media_files for this message

    -- Provenance
    source          TEXT    NOT NULL,              -- live | history
    raw_json        TEXT,                          -- full protobuf message as JSON, for re-processing

    PRIMARY KEY (chat_jid, message_id)
);

CREATE INDEX IF NOT EXISTS idx_messages_sent_at      ON messages (sent_at);
CREATE INDEX IF NOT EXISTS idx_messages_sender_phone ON messages (sender_phone, sent_at);
CREATE INDEX IF NOT EXISTS idx_messages_sender       ON messages (sender, sent_at);

-- One row per media file. Each references the message it belongs to.
CREATE TABLE IF NOT EXISTS message_media_files (
    id                INTEGER PRIMARY KEY,
    chat_jid          TEXT    NOT NULL,            -- with message_id: the messages row this file belongs to
    message_id        TEXT    NOT NULL,
    source_message_id TEXT    NOT NULL,            -- WhatsApp ID of the message that carried the file; for album
                                                   -- items this is the item's own ID, otherwise equal to message_id
    position          INTEGER NOT NULL DEFAULT 0,  -- order within an album (0 for single-file messages)

    media_type        TEXT    NOT NULL,            -- image | video | audio | voice | document | sticker
    caption           TEXT,                        -- this file's caption, if any
    file_name         TEXT,                        -- original file name (documents only)
    relative_path     TEXT,                        -- saved file, relative to the media dir, '/'-separated,
                                                   -- e.g. 20261012T053508Z_3A37298F72373DFA1114.jpg
    mime              TEXT,
    size              INTEGER,                     -- bytes
    sha256            TEXT,                        -- hex sha256 of the saved file
    error             TEXT,                        -- set if the download failed (relative_path is then NULL)
    ingested_at       TEXT    NOT NULL,            -- RFC3339 UTC

    FOREIGN KEY (chat_jid, message_id) REFERENCES messages (chat_jid, message_id) ON DELETE CASCADE,
    UNIQUE (chat_jid, source_message_id)
);

CREATE INDEX IF NOT EXISTS idx_media_files_message ON message_media_files (chat_jid, message_id, position);

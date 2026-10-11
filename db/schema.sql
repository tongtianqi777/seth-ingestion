-- seth-ingestion message store (SQLite).
-- Applied automatically on startup; safe to run repeatedly.
-- Columns added after the first release are also added to older databases by db.Open (see migrate in db.go).

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
    message_type    TEXT    NOT NULL,              -- text | image | edit | other
    body            TEXT,                          -- text body, or image caption
    edit_target_id  TEXT,                          -- for message_type = 'edit': the edited message's ID
    quoted_id       TEXT,                          -- ID of the message this one replies to, if any

    -- Media (images only)
    media_path      TEXT,                          -- absolute path of the saved file
    media_mime      TEXT,
    media_size      INTEGER,
    media_sha256    TEXT,                          -- hex sha256 of the saved file
    media_error     TEXT,                          -- set if the download failed

    -- Provenance
    source          TEXT    NOT NULL,              -- live | history
    raw_json        TEXT,                          -- full protobuf message as JSON, for re-processing

    PRIMARY KEY (chat_jid, message_id)
);

CREATE INDEX IF NOT EXISTS idx_messages_sent_at      ON messages (sent_at);
CREATE INDEX IF NOT EXISTS idx_messages_sender_phone ON messages (sender_phone, sent_at);
CREATE INDEX IF NOT EXISTS idx_messages_sender       ON messages (sender, sent_at);

CREATE TABLE stickers (
    code text PRIMARY KEY,
    label text NOT NULL DEFAULT '',
    media_id uuid REFERENCES media_objects(id)
);
CREATE TABLE messages (
    conversation_id bigint NOT NULL REFERENCES conversations(id),
    seq bigint NOT NULL CHECK (seq > 0),
    sender_id bigint NOT NULL REFERENCES accounts(id),
    kind text NOT NULL CHECK (kind IN ('text','image','voice','file','sticker','nudge')),
    body text NOT NULL DEFAULT '',
    attachment_id uuid REFERENCES media_objects(id),
    duration_ms integer,
    reply_seq bigint CHECK (reply_seq > 0),
    mention_ids bigint[] NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    recalled_at timestamptz,
    recalled_by bigint REFERENCES accounts(id),
    PRIMARY KEY (conversation_id,seq),
    FOREIGN KEY (conversation_id,reply_seq) REFERENCES messages(conversation_id,seq)
);
CREATE INDEX messages_attachment ON messages(attachment_id) WHERE attachment_id IS NOT NULL AND recalled_at IS NULL;
CREATE INDEX messages_sender ON messages(conversation_id,sender_id,seq);
CREATE TABLE featured_messages (
    conversation_id bigint NOT NULL,
    seq bigint NOT NULL,
    added_by bigint NOT NULL REFERENCES accounts(id),
    added_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (conversation_id,seq),
    FOREIGN KEY (conversation_id,seq) REFERENCES messages(conversation_id,seq)
);

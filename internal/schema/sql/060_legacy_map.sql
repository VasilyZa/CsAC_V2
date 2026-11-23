CREATE TABLE legacy_migration (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    fingerprint text NOT NULL CHECK (fingerprint ~ '^[0-9a-f]{64}$'),
    review jsonb NOT NULL,
    imported_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE legacy_message_map (
    source_table text NOT NULL CHECK (source_table IN ('chat_msg', 'private_msg')),
    source_id bigint NOT NULL CHECK (source_id > 0),
    conversation_id bigint NOT NULL,
    seq bigint NOT NULL,
    PRIMARY KEY (source_table, source_id),
    UNIQUE (conversation_id, seq),
    FOREIGN KEY (conversation_id, seq) REFERENCES messages(conversation_id, seq) ON DELETE CASCADE
);

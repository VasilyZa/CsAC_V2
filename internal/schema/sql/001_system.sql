CREATE TABLE event_clock (singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton), head bigint NOT NULL DEFAULT 0, retained_through bigint NOT NULL DEFAULT 0);
INSERT INTO event_clock(singleton) VALUES (true);
CREATE TABLE events (
    seq bigint PRIMARY KEY,
    kind text NOT NULL,
    recipients bigint[] NOT NULL CHECK (cardinality(recipients) > 0),
    data jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX events_recipients ON events USING gin(recipients);
CREATE INDEX events_created_at ON events(created_at);
CREATE TABLE command_keys (
    user_id bigint NOT NULL,
    key text NOT NULL,
    digest bytea NOT NULL,
    result jsonb,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (user_id, key)
);
CREATE INDEX command_keys_created_at ON command_keys(created_at);

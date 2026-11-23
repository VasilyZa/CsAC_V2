-- The deleter's own notice identifies one deletion. Notice history does not
-- grant undo unless the pair also has this authoritative record.
CREATE TABLE friend_deletions (
    notification_id bigint PRIMARY KEY REFERENCES notifications(id) ON DELETE CASCADE,
    user_low bigint NOT NULL REFERENCES accounts(id),
    user_high bigint NOT NULL REFERENCES accounts(id),
    deleted_by bigint NOT NULL,
    friendship_created_at timestamptz NOT NULL,
    UNIQUE (user_low, user_high),
    CHECK (user_low < user_high AND deleted_by IN (user_low, user_high))
);

ALTER TABLE conversations
    ADD COLUMN fixed_code text,
    ADD CONSTRAINT conversations_fixed_code_valid CHECK (
        fixed_code IS NULL OR (kind = 'group' AND octet_length(fixed_code) BETWEEN 1 AND 128)
    );

CREATE INDEX conversations_group_owner ON conversations(owner_id, id DESC)
    WHERE kind = 'group' AND archived_at IS NULL;

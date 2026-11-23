ALTER TABLE space_posts
    ADD COLUMN reply_to_id bigint,
    ADD CONSTRAINT space_reply_target_present CHECK (
        reply_to_id IS NULL OR (parent_id IS NOT NULL AND reply_to_id <> id)
    ),
    ADD CONSTRAINT space_reply_target_key UNIQUE (parent_id, id),
    ADD CONSTRAINT space_reply_target_same_thread FOREIGN KEY (parent_id, reply_to_id)
        REFERENCES space_posts(parent_id, id) ON DELETE SET NULL (reply_to_id);

CREATE INDEX space_replies_by_target ON space_posts(parent_id, reply_to_id) WHERE reply_to_id IS NOT NULL;

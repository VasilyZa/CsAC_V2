-- A deletion can remain evidence for a consensual recovery after its
-- authorization for a unilateral undo has been invalidated.
ALTER TABLE friend_deletions
    ADD COLUMN undo_allowed boolean NOT NULL DEFAULT true,
    ADD COLUMN original_time_verified boolean NOT NULL DEFAULT false;

ALTER TABLE friend_requests
    ADD COLUMN kind text NOT NULL DEFAULT 'add' CHECK (kind IN ('add', 'recovery')),
    ADD COLUMN recovery_friendship_created_at timestamptz,
    ADD CONSTRAINT friend_requests_recovery_time_check CHECK (
        recovery_friendship_created_at IS NULL OR (
            kind = 'recovery'
            AND isfinite(recovery_friendship_created_at)
            AND recovery_friendship_created_at <= created_at
        )
    );

CREATE INDEX friend_requests_recovery_recent
    ON friend_requests(sender_id, recipient_id, created_at DESC) WHERE kind = 'recovery';

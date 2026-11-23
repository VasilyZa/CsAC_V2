ALTER TABLE messages ADD COLUMN recall_kind text CHECK (
    recall_kind IS NULL OR (recalled_at IS NOT NULL AND recall_kind IN ('sender','admin','owner'))
);

-- A sender's own recall is provable. The current group role does not prove
-- which role an existing moderator held at the time of a historical recall.
UPDATE messages SET recall_kind='sender'
WHERE recalled_at IS NOT NULL AND recalled_by=sender_id;

CREATE FUNCTION chat_recall_kind(sender bigint, viewer bigint, sender_role text, viewer_role text,
    sent_at timestamptz, as_of timestamptz) RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
RETURN CASE
    WHEN sender=viewer AND (viewer_role='owner' OR sent_at>=as_of-interval '2 minutes') THEN 'sender'
    WHEN viewer_role='owner' THEN 'owner'
    WHEN viewer_role='admin' AND sender_role='member' AND sender<>viewer THEN 'admin'
END;

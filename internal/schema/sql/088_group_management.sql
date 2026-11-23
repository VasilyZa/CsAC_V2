CREATE VIEW active_site_admins AS
SELECT id AS user_id FROM accounts WHERE site_admin
AND (status='active' OR (status='banned' AND ban_until<=statement_timestamp()));

-- Review access is separate from membership and the real-time delivery audience.
CREATE VIEW chat_reviewers AS
SELECT conversation_id,user_id FROM chat_readers
UNION ALL
SELECT c.id,a.user_id FROM conversations c CROSS JOIN active_site_admins a
WHERE c.kind='group' AND c.archived_at IS NULL;

CREATE INDEX conversations_manage_directory ON conversations(id) WHERE kind='group' AND archived_at IS NULL;

CREATE FUNCTION chat_recall_kind(sender bigint, viewer bigint, message_kind text, sender_is_owner boolean, group_role text,
    site_manager boolean, sent_at timestamptz, as_of timestamptz) RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
RETURN CASE
    WHEN message_kind='system' THEN NULL
    WHEN sender=viewer AND (sent_at>=as_of-interval '2 minutes' OR
        (message_kind IN ('text','image','voice','file','sticker') AND (site_manager OR group_role IN ('owner','admin')))) THEN 'sender'
    WHEN message_kind NOT IN ('text','image','voice','file','sticker') THEN NULL
    WHEN group_role='owner' THEN 'owner'
    WHEN site_manager OR (group_role='admin' AND NOT sender_is_owner) THEN 'admin'
END;

ALTER TABLE messages DROP CONSTRAINT messages_kind_check,
    ADD CONSTRAINT messages_kind_check CHECK (kind IN ('text','image','voice','file','sticker','nudge','system')),
    ADD COLUMN system_action text,
    ADD COLUMN system_subject_id bigint REFERENCES accounts(id),
    ADD CONSTRAINT messages_system_notice CHECK (
        (kind<>'system' AND system_action IS NULL AND system_subject_id IS NULL) OR
        (kind='system' AND system_action IS NOT NULL AND system_action IN ('feature_added','feature_removed')
          AND system_subject_id IS NOT NULL)
    );

CREATE INDEX messages_purge_system_subject ON messages(system_subject_id,conversation_id,seq) WHERE system_subject_id IS NOT NULL;

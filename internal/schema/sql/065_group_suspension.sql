ALTER TABLE conversations
    ADD COLUMN site_ban_until timestamptz,
    ADD COLUMN site_ban_reason text NOT NULL DEFAULT '',
    ADD CONSTRAINT conversations_site_ban_valid CHECK (
        (site_ban_until IS NULL AND site_ban_reason = '')
        OR (kind = 'group' AND site_ban_until IS NOT NULL AND char_length(site_ban_reason) BETWEEN 1 AND 500)
    );

CREATE INDEX conversations_site_ban_list ON conversations (id DESC)
    WHERE kind = 'group' AND archived_at IS NULL AND site_ban_until IS NOT NULL;

-- Apply the history restriction to queued and replayed chat events too.
CREATE OR REPLACE VIEW chat_readers AS
SELECT c.id AS conversation_id, member.user_id
FROM conversations c
JOIN conversation_members member ON member.conversation_id=c.id
WHERE c.archived_at IS NULL
  AND (c.site_ban_until IS NULL OR c.site_ban_until<=statement_timestamp())
  AND (c.kind='group' OR (
    EXISTS (SELECT 1 FROM friendships f WHERE f.user_low=c.direct_low AND f.user_high=c.direct_high)
    AND EXISTS (SELECT 1 FROM accounts peer
        WHERE peer.id=CASE WHEN member.user_id=c.direct_low THEN c.direct_high ELSE c.direct_low END
          AND (peer.status='active' OR (peer.status='banned' AND peer.ban_until<=now())))
    AND NOT EXISTS (SELECT 1 FROM friend_preferences pref WHERE pref.blocked AND
       ((pref.owner_id=c.direct_low AND pref.other_id=c.direct_high) OR
        (pref.owner_id=c.direct_high AND pref.other_id=c.direct_low)))
  ));

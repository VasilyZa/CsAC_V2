ALTER TABLE events ADD COLUMN conversation_id bigint REFERENCES conversations(id);
ALTER TABLE events ADD COLUMN message_seq bigint;
ALTER TABLE events ADD CONSTRAINT events_message_scope_fk
    FOREIGN KEY(conversation_id, message_seq) REFERENCES messages(conversation_id, seq);
ALTER TABLE events ADD CONSTRAINT events_message_requires_conversation
    CHECK (message_seq IS NULL OR conversation_id IS NOT NULL);

-- A queued or replayed chat event must still be readable when it is sent.
CREATE VIEW chat_readers AS
SELECT c.id AS conversation_id, member.user_id
FROM conversations c
JOIN conversation_members member ON member.conversation_id=c.id
WHERE c.archived_at IS NULL
  AND (c.kind='group' OR (
    EXISTS (SELECT 1 FROM friendships f WHERE f.user_low=c.direct_low AND f.user_high=c.direct_high)
    AND EXISTS (SELECT 1 FROM accounts peer
        WHERE peer.id=CASE WHEN member.user_id=c.direct_low THEN c.direct_high ELSE c.direct_low END
          AND (peer.status='active' OR (peer.status='banned' AND peer.ban_until<=now())))
    AND NOT EXISTS (SELECT 1 FROM friend_preferences pref WHERE pref.blocked AND
       ((pref.owner_id=c.direct_low AND pref.other_id=c.direct_high) OR
        (pref.owner_id=c.direct_high AND pref.other_id=c.direct_low)))
  ));

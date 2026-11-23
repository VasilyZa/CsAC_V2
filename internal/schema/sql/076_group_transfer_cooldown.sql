ALTER TABLE conversations ADD COLUMN owner_transfer_cooldown_until timestamptz
    CHECK (kind='group' OR owner_transfer_cooldown_until IS NULL);

UPDATE conversations c
SET owner_transfer_cooldown_until=t.last_accepted_at+interval '672 hours'
FROM (
    SELECT conversation_id,max(resolved_at) AS last_accepted_at
    FROM group_transfers WHERE status='accepted' GROUP BY conversation_id
) t
WHERE c.id=t.conversation_id AND c.kind='group';

DROP INDEX group_transfers_cooldown;

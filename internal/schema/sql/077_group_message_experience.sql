CREATE TABLE group_member_experience (
    conversation_id bigint NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    experience bigint NOT NULL CHECK (experience >= 0),
    earned_today integer NOT NULL DEFAULT 0 CHECK (earned_today BETWEEN 0 AND 100 AND earned_today % 10 = 0),
    last_credit_slot timestamptz,
    PRIMARY KEY (conversation_id, user_id),
    CHECK ((last_credit_slot IS NULL) = (earned_today = 0)),
    CHECK (experience >= earned_today)
);

CREATE FUNCTION group_experience_day(at_time timestamptz) RETURNS date
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
RETURN (at_time AT TIME ZONE 'Asia/Shanghai')::date;

CREATE FUNCTION group_experience_slot(at_time timestamptz) RETURNS timestamptz
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
RETURN date_bin(interval '15 seconds',at_time,timestamptz '1970-01-01 00:00:00+00');

-- Used only for upgrades and bulk imports into a fresh database. Recalled
-- messages still count because sending, rather than retaining them, earns XP.
CREATE FUNCTION seed_group_experience_from_history() RETURNS void
LANGUAGE sql VOLATILE AS $$
    WITH message_slots AS (
        SELECT m.conversation_id,m.sender_id,group_experience_slot(least(m.created_at,statement_timestamp())) AS slot
        FROM messages m JOIN conversations c ON c.id=m.conversation_id
        WHERE c.kind='group' AND m.kind<>'nudge'
    ), days AS (
        SELECT conversation_id,sender_id,max(slot) AS last_slot,
            least(100,10*count(DISTINCT slot))::integer AS earned
        FROM message_slots GROUP BY conversation_id,sender_id,group_experience_day(slot)
    )
    INSERT INTO group_member_experience(conversation_id,user_id,experience,earned_today,last_credit_slot)
    SELECT DISTINCT ON (conversation_id,sender_id) conversation_id,sender_id,
        sum(earned) OVER (PARTITION BY conversation_id,sender_id),earned,last_slot
    FROM days ORDER BY conversation_id,sender_id,last_slot DESC
    ON CONFLICT DO NOTHING;
$$;

SELECT seed_group_experience_from_history();

-- Preserve the progress of existing v2 members even with incomplete message history.
INSERT INTO group_member_experience AS xp(conversation_id,user_id,experience)
SELECT d.conversation_id,d.user_id,group_activity_points(d.conversation_id,d.user_id)
FROM (SELECT DISTINCT conversation_id,user_id FROM group_member_active_days) d
ON CONFLICT (conversation_id,user_id) DO UPDATE SET experience=greatest(xp.experience,excluded.experience);

UPDATE conversation_members m SET level=group_level_from_points(coalesce((
    SELECT experience FROM group_member_experience e WHERE e.conversation_id=m.conversation_id AND e.user_id=m.user_id
),0))
FROM conversations c WHERE c.id=m.conversation_id AND c.kind='group' AND NOT m.level_custom;

-- The caller already holds the conversation head lock in the message transaction.
CREATE FUNCTION record_group_message_experience(group_id bigint, member_id bigint, sent_at timestamptz, message_kind text)
RETURNS TABLE(display_level smallint,display_title text,total_experience bigint,gained integer)
LANGUAGE sql VOLATILE AS $$
    WITH credit AS (
        INSERT INTO group_member_experience AS xp(conversation_id,user_id,experience,earned_today,last_credit_slot)
        SELECT $1,$2,10,10,group_experience_slot($3) WHERE $4<>'nudge'
        ON CONFLICT (conversation_id,user_id) DO UPDATE SET
            experience=xp.experience+10,
            earned_today=CASE WHEN group_experience_day(xp.last_credit_slot)=group_experience_day(excluded.last_credit_slot)
                THEN xp.earned_today+10 ELSE 10 END,
            last_credit_slot=excluded.last_credit_slot
        WHERE (xp.last_credit_slot IS NULL OR xp.last_credit_slot<excluded.last_credit_slot)
            AND (xp.earned_today<100 OR group_experience_day(xp.last_credit_slot)<group_experience_day(excluded.last_credit_slot))
        RETURNING experience
    ), earned AS MATERIALIZED (
        SELECT group_level_from_points(credit.experience) AS level FROM credit
        JOIN conversation_members m ON m.conversation_id=$1 AND m.user_id=$2 AND NOT m.level_custom
        WHERE credit.experience>=group_activity_threshold(m.level+1)
    ), raised AS (
        UPDATE conversation_members m SET level=earned.level FROM earned
        WHERE m.conversation_id=$1 AND m.user_id=$2 AND m.level<earned.level RETURNING m.level
    )
    SELECT coalesce(raised.level,m.level),coalesce(nullif(m.title,''),group_default_title(coalesce(raised.level,m.level))),
        coalesce(credit.experience,xp.experience,0),CASE WHEN credit.experience IS NULL THEN 0 ELSE 10 END
    FROM conversation_members m LEFT JOIN group_member_experience xp ON xp.conversation_id=m.conversation_id AND xp.user_id=m.user_id
    LEFT JOIN raised ON true LEFT JOIN credit ON true
    WHERE m.conversation_id=$1 AND m.user_id=$2;
$$;

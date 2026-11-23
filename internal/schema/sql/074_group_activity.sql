ALTER TABLE conversation_members ADD COLUMN level_custom boolean NOT NULL DEFAULT false;
UPDATE conversation_members SET level_custom = true WHERE level <> 1;

CREATE TABLE group_member_active_days (
    conversation_id bigint NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    user_id bigint NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    active_on date NOT NULL,
    PRIMARY KEY (conversation_id, user_id, active_on)
);

INSERT INTO group_member_active_days (conversation_id, user_id, active_on)
SELECT DISTINCT m.conversation_id,m.sender_id,(m.created_at AT TIME ZONE 'UTC')::date
FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE c.kind='group';

CREATE FUNCTION group_level_from_days(days bigint) RETURNS smallint
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
RETURN CASE WHEN days >= 2320 THEN least(100,20+(days-2320)/300)::smallint
ELSE (SELECT max(level)::smallint
      FROM unnest(ARRAY[0,7,14,30,60,90,130,180,240,320,420,540,680,840,1020,1220,1450,1710,2000,2320])
      WITH ORDINALITY AS thresholds(required_days,level) WHERE required_days <= days)
END;

CREATE FUNCTION group_default_title(level smallint) RETURNS text
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
RETURN CASE WHEN level <= 10 THEN '青铜' WHEN level <= 20 THEN '白银'
    WHEN level <= 40 THEN '黄金' WHEN level <= 80 THEN '铂金' ELSE '王者' END;

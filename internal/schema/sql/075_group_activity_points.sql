-- Days are the source of truth. Group the unique UTC dates into consecutive runs.
-- added_day comes from INSERT RETURNING and is outside the caller's read snapshot.
CREATE FUNCTION group_activity_points(group_id bigint, member_id bigint, added_day date DEFAULT NULL)
RETURNS bigint LANGUAGE sql STABLE PARALLEL SAFE AS $$
    WITH dates AS (
        SELECT active_on FROM group_member_active_days WHERE conversation_id=$1 AND user_id=$2
        UNION ALL SELECT $3 WHERE $3 IS NOT NULL
    ), runs AS (
        SELECT active_on - row_number() OVER (ORDER BY active_on)::integer AS run_start
        FROM dates
    ), run_lengths AS (
        SELECT count(*) AS days FROM runs GROUP BY run_start
    )
    SELECT coalesce(sum(10*days + 2*(days/7)),0)::bigint FROM run_lengths;
$$;

-- The first step costs 30 points, then each step costs 3 points more.
CREATE FUNCTION group_activity_threshold(level integer) RETURNS integer
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
RETURN CASE WHEN level BETWEEN 1 AND 100 THEN 3*(level-1)*(level+18)/2 END;

CREATE FUNCTION group_level_from_points(points bigint) RETURNS smallint
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE
RETURN (SELECT max(level)::smallint FROM generate_series(1,100) AS levels(level)
    WHERE group_activity_threshold(level) <= points);

UPDATE conversation_members m
SET level=group_level_from_points(group_activity_points(m.conversation_id,m.user_id))
FROM conversations c
WHERE c.id=m.conversation_id AND c.kind='group' AND NOT m.level_custom;

DROP FUNCTION group_level_from_days(bigint);

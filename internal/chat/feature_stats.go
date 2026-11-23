package chat

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"csac/v2/internal/rpc"
)

const defaultFeatureTimeZone = "Asia/Shanghai"

type featureStatsInput struct {
	conversationRef
	Period   string `json:"period"`
	TimeZone string `json:"time_zone"`
}

type featureRank struct {
	Rank       int    `json:"rank"`
	SenderID   int64  `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Count      int64  `json:"count"`
}

type featureStatsResult struct {
	Period        string           `json:"period"`
	TimeZone      string           `json:"time_zone"`
	Count         int64            `json:"count"`
	CountsByKind  map[string]int64 `json:"counts_by_kind"`
	Rank          []featureRank    `json:"rank"`
	LatestAddedAt *time.Time       `json:"latest_added_at"`
}

func featureStats(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[featureStatsInput](raw)
	if err != nil {
		return nil, err
	}
	switch in.Period {
	case "":
		in.Period = "all"
	case "today", "week", "month", "all":
	default:
		return nil, rpc.Invalid("Unknown feature period")
	}
	if in.TimeZone == "" {
		in.TimeZone = defaultFeatureTimeZone
	}
	if _, err := accessTo(ctx, call.DB, in.ConversationID, call.Actor.ID, false); err != nil {
		return nil, err
	}
	if in.TimeZone != defaultFeatureTimeZone && in.TimeZone != "UTC" {
		// Regional names avoid ambiguous time zone abbreviations in PostgreSQL.
		if len(in.TimeZone) > 128 || !strings.Contains(in.TimeZone, "/") || strings.ContainsRune(in.TimeZone, 0) {
			return nil, rpc.Invalid("Unknown feature time zone")
		}
		var known bool
		if err := call.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_timezone_names WHERE name=$1)`, in.TimeZone).Scan(&known); err != nil {
			return nil, err
		}
		if !known {
			return nil, rpc.Invalid("Unknown feature time zone")
		}
	}
	return summarizeFeatures(ctx, call.DB, call.Actor.ID, in, nil)
}

// An explicit as-of instant lets tests exercise both sides of historical wall
// time transitions. Live calls use the start of the PostgreSQL statement.
func summarizeFeatures(ctx context.Context, db rpc.DB, user int64, in featureStatsInput, asOf *time.Time) (featureStatsResult, error) {
	out := featureStatsResult{Period: in.Period, TimeZone: in.TimeZone}
	var text, image, voice, file, sticker, nudge int64
	// Comparing local dates handles days that start with a skipped or repeated
	// wall-clock hour. The rolling windows measure elapsed time in hours.
	err := db.QueryRow(ctx, `WITH bounds AS (
		SELECT coalesce($5::timestamptz,statement_timestamp()) AS as_of
	), features AS MATERIALIZED (
		SELECT m.sender_id,m.kind,f.added_at,CASE $3::text
			WHEN 'today' THEN (f.added_at AT TIME ZONE $4::text)::date=(b.as_of AT TIME ZONE $4::text)::date
			WHEN 'week' THEN f.added_at>=b.as_of-interval '168 hours'
			WHEN 'month' THEN f.added_at>=b.as_of-interval '720 hours'
			ELSE true
		END AS in_period
		FROM featured_messages f JOIN messages m USING (conversation_id,seq) CROSS JOIN bounds b
		WHERE f.conversation_id=$1 AND m.recalled_at IS NULL AND f.added_at<=b.as_of
		AND EXISTS (SELECT 1 FROM chat_readers r WHERE r.conversation_id=$1 AND r.user_id=$2)
	), ranked AS (
		SELECT sender_id,count(*) AS count FROM features WHERE in_period
		GROUP BY sender_id ORDER BY count DESC,sender_id LIMIT 10
	)
	SELECT count(*) FILTER (WHERE in_period),
		count(*) FILTER (WHERE in_period AND kind='text'),
		count(*) FILTER (WHERE in_period AND kind='image'),
		count(*) FILTER (WHERE in_period AND kind='voice'),
		count(*) FILTER (WHERE in_period AND kind='file'),
		count(*) FILTER (WHERE in_period AND kind='sticker'),
		count(*) FILTER (WHERE in_period AND kind='nudge'),
		max(added_at),
		(SELECT coalesce(jsonb_agg(jsonb_build_object('sender_id',r.sender_id,'sender_name',a.nickname,'count',r.count)
			ORDER BY r.count DESC,r.sender_id),'[]'::jsonb)
		FROM ranked r JOIN accounts a ON a.id=r.sender_id)
	FROM features`, in.ConversationID, user, in.Period, in.TimeZone, asOf).
		Scan(&out.Count, &text, &image, &voice, &file, &sticker, &nudge, &out.LatestAddedAt, &out.Rank)
	if err != nil {
		return out, err
	}
	out.CountsByKind = map[string]int64{"text": text, "image": image, "voice": voice, "file": file, "sticker": sticker, "nudge": nudge}
	for i := range out.Rank {
		out.Rank[i].Rank = i + 1
	}
	if out.LatestAddedAt != nil {
		utc := out.LatestAddedAt.UTC()
		out.LatestAddedAt = &utc
	}
	return out, nil
}

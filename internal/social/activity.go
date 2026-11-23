package social

import (
	"context"
	"encoding/json"
	"time"

	"csac/v2/internal/rpc"
)

type activityProgress struct {
	Level       int16          `json:"level"`
	EarnedLevel int16          `json:"earned_level"`
	Experience  int64          `json:"experience"`
	ManualLevel bool           `json:"manual_level"`
	Progress    *levelProgress `json:"progress"`
	Daily       dailyXP        `json:"daily"`
}

type levelProgress struct {
	Current  int64 `json:"current"`
	Required int32 `json:"required"`
}

type dailyXP struct {
	Earned   int32     `json:"earned"`
	Limit    int32     `json:"limit"`
	ResetsAt time.Time `json:"resets_at"`
}

func groupActivity(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[groupInput](raw)
	if err != nil {
		return nil, err
	}
	if in.ConversationID <= 0 {
		return nil, rpc.Invalid("conversation_id must be positive")
	}
	var result activityProgress
	var threshold int32
	var next *int32
	err = call.DB.QueryRow(ctx, `WITH status AS (
		SELECT m.level,m.level_custom,coalesce(xp.experience,0) AS experience,
			CASE WHEN m.level_custom THEN group_level_from_points(coalesce(xp.experience,0)) ELSE m.level END AS earned_level,
			CASE WHEN group_experience_day(xp.last_credit_slot)=group_experience_day(statement_timestamp())
				THEN xp.earned_today ELSE 0 END AS earned_today
		FROM conversation_members m JOIN conversations c ON c.id=m.conversation_id
		LEFT JOIN group_member_experience xp ON xp.conversation_id=m.conversation_id AND xp.user_id=m.user_id
		WHERE m.conversation_id=$1 AND m.user_id=$2 AND c.kind='group' AND c.archived_at IS NULL
	)
	SELECT level,earned_level,experience,level_custom,group_activity_threshold(earned_level),group_activity_threshold(earned_level+1),
		earned_today,(group_experience_day(statement_timestamp())+1)::timestamp AT TIME ZONE 'Asia/Shanghai'
	FROM status`, in.ConversationID, call.Actor.ID).
		Scan(&result.Level, &result.EarnedLevel, &result.Experience, &result.ManualLevel, &threshold, &next, &result.Daily.Earned, &result.Daily.ResetsAt)
	if err != nil {
		return nil, err
	}
	if next != nil {
		result.Progress = &levelProgress{Current: result.Experience - int64(threshold), Required: *next - threshold}
	}
	result.Daily.Limit = 100
	result.Daily.ResetsAt = result.Daily.ResetsAt.UTC()
	return result, nil
}

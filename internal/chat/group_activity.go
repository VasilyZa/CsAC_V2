package chat

import (
	"context"
	"time"

	"csac/v2/internal/rpc"
)

type groupActivity struct {
	MemberLevel int16  `json:"member_level"`
	MemberTitle string `json:"member_title"`
}

type experienceAward struct {
	Total  int64 `json:"total"`
	Gained int   `json:"gained"`
}

func recordGroupActivity(ctx context.Context, db rpc.DB, room, user int64, at time.Time, kind string) (*groupActivity, *experienceAward, error) {
	var rank groupActivity
	var award experienceAward
	err := db.QueryRow(ctx, `SELECT display_level,display_title,total_experience,gained
		FROM record_group_message_experience($1,$2,$3,$4)`, room, user, at, kind).
		Scan(&rank.MemberLevel, &rank.MemberTitle, &award.Total, &award.Gained)
	return &rank, &award, err
}

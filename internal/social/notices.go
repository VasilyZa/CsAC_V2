package social

import (
	"context"
	"encoding/json"
	"time"

	"csac/v2/internal/rpc"

	"github.com/jackc/pgx/v5"
)

type noticeItem struct {
	ID        int64           `json:"id"`
	Kind      string          `json:"kind"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"created_at"`
	ReadAt    *time.Time      `json:"read_at"`
	CanUndo   *bool           `json:"can_undo,omitempty"`
}

const recentDeletionNoticeSQL = `(n.created_at>statement_timestamp()-interval '72 hours' AND n.created_at<=statement_timestamp())`
const visibleNoticeSQL = `(n.kind<>'friend.deleted' OR ` + recentDeletionNoticeSQL + `)`
const noticeInboxSQL = `n.user_id=$1 AND ($2='' OR n.kind=$2) AND ` + visibleNoticeSQL

func listNotices(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		pageInput
		Kind       string `json:"kind"`
		UnreadOnly bool   `json:"unread_only"`
	}](raw)
	if err != nil {
		return nil, err
	}
	limit, err := in.size()
	if err != nil {
		return nil, err
	}
	kind, err := cleanText("kind", in.Kind, 64, false)
	if err != nil {
		return nil, err
	}
	items, err := collect(ctx, call.DB, `SELECT n.id,n.kind,n.data,n.created_at,n.read_at,
		CASE WHEN n.kind='friend.deleted' THEN $6::boolean AND d.undo_allowed IS TRUE AND d.friendship_created_at<=n.created_at
			AND EXISTS(SELECT 1 FROM accounts a WHERE a.id=CASE WHEN d.user_low=n.user_id THEN d.user_high ELSE d.user_low END
			AND `+eligibleAccount+`) END
		FROM notifications n LEFT JOIN friend_deletions d ON d.notification_id=n.id AND d.deleted_by=n.user_id
		WHERE `+noticeInboxSQL+` AND ($3::bigint=0 OR n.id<$3) AND (NOT $4::boolean OR n.read_at IS NULL)
		ORDER BY n.id DESC LIMIT $5`, pgx.RowToStructByPos[noticeItem], call.Actor.ID, kind, in.BeforeID, in.UnreadOnly, limit+1, call.Actor.Chat)
	return paginate(items, limit, func(item noticeItem) int64 { return item.ID }), err
}

func countNotices(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	if _, err := rpc.Decode[struct{}](raw); err != nil {
		return nil, err
	}
	var unread, system, pendingRequests, deletions int64
	err := call.DB.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE n.kind NOT IN ('friend.request','friend.deleted')),
		(SELECT count(*) FROM friend_requests WHERE recipient_id=$1 AND status='pending'),
		count(*) FILTER (WHERE n.kind='friend.deleted')
		FROM notifications n WHERE n.user_id=$1 AND n.read_at IS NULL AND `+visibleNoticeSQL, call.Actor.ID).
		Scan(&unread, &system, &pendingRequests, &deletions)
	return fields{"unread": unread, "system_unread": system, "friend_requests_pending": pendingRequests,
		"friend_deletions_unread": deletions, "total_unread": system + pendingRequests + deletions}, err
}

func readNotices(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		Kind           string `json:"kind"`
		NotificationID *int64 `json:"notification_id"`
		All            bool   `json:"all"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.All == (in.NotificationID != nil) || (in.NotificationID != nil && *in.NotificationID <= 0) {
		return nil, rpc.Invalid("Supply notification_id or all=true")
	}
	kind, err := cleanText("kind", in.Kind, 64, false)
	if err != nil {
		return nil, err
	}
	update := `UPDATE notifications n SET read_at=clock_timestamp() WHERE ` + noticeInboxSQL + ` AND n.read_at IS NULL`
	var count int64
	if in.All {
		tag, err := call.DB.Exec(ctx, update, call.Actor.ID, kind)
		if err != nil {
			return nil, err
		}
		count = tag.RowsAffected()
	} else {
		var found bool
		err := call.DB.QueryRow(ctx, `WITH changed AS (`+update+` AND n.id=$3 RETURNING id)
			SELECT (SELECT count(*) FROM changed), EXISTS(SELECT 1 FROM notifications n WHERE `+noticeInboxSQL+` AND n.id=$3)`,
			call.Actor.ID, kind, *in.NotificationID).Scan(&count, &found)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, rpc.NotFound("Notification not found")
		}
	}
	if count != 0 {
		if err := call.Emit("notices.changed", []int64{call.Actor.ID}, fields{}); err != nil {
			return nil, err
		}
	}
	return fields{"updated": count}, nil
}

func notify(ctx context.Context, call rpc.Call, users []int64, kind string, payload fields) error {
	if len(users) == 0 {
		return nil
	}
	if _, err := call.DB.Exec(ctx, `INSERT INTO notifications(user_id,kind,data)
		SELECT u,$2,$3::jsonb FROM unnest($1::bigint[]) AS u`, users, kind, payload); err != nil {
		return err
	}
	return call.Emit("notices.changed", users, fields{})
}

func readActionNotice(ctx context.Context, call rpc.Call, userID int64, kind string, key fields) error {
	tag, err := call.DB.Exec(ctx, `UPDATE notifications SET read_at=clock_timestamp()
		WHERE user_id=$1 AND read_at IS NULL AND kind=$2 AND data @> $3::jsonb`, userID, kind, key)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return call.Emit("notices.changed", []int64{userID}, fields{})
}

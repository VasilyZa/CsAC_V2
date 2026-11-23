package social

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"csac/v2/internal/rpc"

	"github.com/jackc/pgx/v5"
)

const pairBlockedSQL = `SELECT EXISTS(SELECT 1 FROM friend_preferences
	WHERE blocked AND ((owner_id=$1 AND other_id=$2) OR (owner_id=$2 AND other_id=$1)))`

const (
	dailyFriendRequestLimit = 30
	friendRequestAdd        = "add"
	friendRequestRecovery   = "recovery"
)
const recentFriendRequestsSQL = `(SELECT count(*) FROM friend_requests
	WHERE sender_id=$1 AND created_at>clock_timestamp()-interval '24 hours')`
const pendingFriendPairSQL = `r.status='pending' AND LEAST(r.sender_id,r.recipient_id)=LEAST($1::bigint,$2::bigint)
	AND GREATEST(r.sender_id,r.recipient_id)=GREATEST($1::bigint,$2::bigint)`

func pairBlocked(ctx context.Context, db rpc.DB, first, second int64) (bool, error) {
	var blocked bool
	err := db.QueryRow(ctx, pairBlockedSQL, first, second).Scan(&blocked)
	return blocked, err
}

// The canonical transaction lock also covers pairs with no relationship rows yet.
func lockFriendPair(ctx context.Context, db rpc.DB, first, second int64) error {
	_, err := db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('social:pair:'||$1::bigint::text||':'||$2::bigint::text, 0))`, min(first, second), max(first, second))
	return err
}

// Chat locks an existing friendship in share mode before a direct write.
// Block changes take the opposing row lock even though the edge survives.
func lockStoredFriendship(ctx context.Context, db rpc.DB, first, second int64) error {
	_, err := db.Exec(ctx, `SELECT user_low FROM friendships WHERE user_low=$1 AND user_high=$2 FOR UPDATE`, min(first, second), max(first, second))
	return err
}

func requestFriend(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	return createFriendRequest(ctx, call, raw, friendRequestAdd)
}

func recoverFriend(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	return createFriendRequest(ctx, call, raw, friendRequestRecovery)
}

func createFriendRequest(ctx context.Context, call rpc.Call, raw json.RawMessage, kind string) (any, error) {
	in, err := rpc.Decode[struct {
		userInput
		Message string `json:"message"`
	}](raw)
	if err != nil {
		return nil, err
	}
	actor, target := call.Actor.ID, in.UserID
	if err := otherUser(actor, target); err != nil {
		return nil, err
	}
	message, err := cleanText("message", in.Message, 500, false)
	if err != nil {
		return nil, err
	}
	if err := activeAccount(ctx, call.DB, target); err != nil {
		return nil, err
	}
	// Serialize the daily sender quota independently of the target pair.
	if _, err := call.DB.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('social:request:'||$1::bigint::text, 0))`, actor); err != nil {
		return nil, err
	}
	if err := lockFriendPair(ctx, call.DB, actor, target); err != nil {
		return nil, err
	}
	blocked, err := pairBlocked(ctx, call.DB, actor, target)
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, rpc.Denied("Friend request unavailable")
	}
	var friends bool
	if err := call.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM friendships WHERE user_low=$1 AND user_high=$2)`, min(actor, target), max(actor, target)).Scan(&friends); err != nil {
		return nil, err
	}
	if friends {
		return nil, rpc.Conflict("Already friends")
	}
	var recent int
	if err := call.DB.QueryRow(ctx, `SELECT `+recentFriendRequestsSQL, actor).Scan(&recent); err != nil {
		return nil, err
	}
	if recent >= dailyFriendRequestLimit {
		return nil, rpc.RateLimited("Daily friend request limit reached")
	}
	var originalTime *time.Time
	if kind == friendRequestRecovery {
		originalTime, err = validateFriendRecovery(ctx, call.DB, actor, target)
		if err != nil {
			return nil, err
		}
	}
	var id int64
	err = call.DB.QueryRow(ctx, `INSERT INTO friend_requests(sender_id,recipient_id,message,kind,recovery_friendship_created_at)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING RETURNING id`, actor, target, message, kind, originalTime).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, rpc.Conflict("A friend request is already pending")
	}
	if err != nil {
		return nil, err
	}
	if err := invalidateFriendUndo(ctx, call.DB, actor, target); err != nil {
		return nil, err
	}
	return fields{"request_id": id, "status": pending, "kind": kind}, notify(ctx, call, []int64{target}, "friend.request",
		fields{"request_id": id, "user_id": actor, "kind": kind})
}

// The caller holds the pair lock. The deleter's notice and its authoritative
// pair record must agree. Only a verified original timestamp is copied onto
// the request so the recipient can later decide it after the deletion window.
func validateFriendRecovery(ctx context.Context, db rpc.DB, sender, recipient int64) (*time.Time, error) {
	var originalTime *time.Time
	var recentRequest bool
	err := db.QueryRow(ctx, `SELECT CASE WHEN d.original_time_verified THEN d.friendship_created_at END,
		EXISTS(SELECT 1 FROM friend_requests r
			WHERE r.sender_id=$1 AND r.recipient_id=$2 AND r.kind='recovery'
			AND r.created_at>statement_timestamp()-interval '24 hours' AND r.created_at<=statement_timestamp())
		FROM friend_deletions d JOIN notifications n ON n.id=d.notification_id
		WHERE d.user_low=LEAST($1::bigint,$2::bigint) AND d.user_high=GREATEST($1::bigint,$2::bigint)
		AND n.user_id=d.deleted_by AND n.kind='friend.deleted' AND `+recentDeletionNoticeSQL+`
		AND isfinite(d.friendship_created_at) AND d.friendship_created_at<=n.created_at
		AND n.data @> jsonb_build_object('deleted_by',d.deleted_by,'user_id',
			CASE WHEN d.deleted_by=d.user_low THEN d.user_high ELSE d.user_low END)`, sender, recipient).
		Scan(&originalTime, &recentRequest)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, rpc.Conflict("No eligible recent friendship deletion")
	}
	if err != nil {
		return nil, err
	}
	if recentRequest {
		return nil, rpc.RateLimited("A recovery request was sent to this user in the last 24 hours")
	}
	return originalTime, nil
}

func respondFriend(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		RequestID int64 `json:"request_id"`
		Accept    *bool `json:"accept"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.RequestID <= 0 || in.Accept == nil {
		return nil, rpc.Invalid("request_id and accept are required")
	}
	actor := call.Actor.ID
	var sender int64
	if err := call.DB.QueryRow(ctx, `SELECT sender_id FROM friend_requests WHERE id=$1 AND recipient_id=$2`, in.RequestID, actor).Scan(&sender); err != nil {
		return nil, err
	}
	if err := lockFriendPair(ctx, call.DB, sender, actor); err != nil {
		return nil, err
	}
	var current, kind string
	var originalTime *time.Time
	if err := call.DB.QueryRow(ctx, `SELECT status,kind,recovery_friendship_created_at FROM friend_requests WHERE id=$1`, in.RequestID).
		Scan(&current, &kind, &originalTime); err != nil {
		return nil, err
	}
	if current != pending {
		return nil, rpc.Conflict("Friend request has already been resolved")
	}
	status := responseStatus(in.Accept)
	if status == accepted {
		if err := activeAccount(ctx, call.DB, sender); err != nil {
			return nil, err
		}
		blocked, err := pairBlocked(ctx, call.DB, sender, actor)
		if err != nil {
			return nil, err
		}
		if blocked {
			return nil, rpc.Denied("Friend request unavailable")
		}
		if _, err := call.DB.Exec(ctx, `INSERT INTO friendships(user_low,user_high,created_at)
			VALUES($1,$2,COALESCE($3::timestamptz,clock_timestamp())) ON CONFLICT DO NOTHING`, min(sender, actor), max(sender, actor), originalTime); err != nil {
			return nil, err
		}
		if err := friendsChanged(call, actor, sender); err != nil {
			return nil, err
		}
	}
	if _, err := call.DB.Exec(ctx, `UPDATE friend_requests SET status=$2,resolved_at=clock_timestamp() WHERE id=$1`, in.RequestID, status); err != nil {
		return nil, err
	}
	if status == accepted {
		if err := clearFriendDeletion(ctx, call.DB, actor, sender); err != nil {
			return nil, err
		}
	} else if err := invalidateFriendUndo(ctx, call.DB, actor, sender); err != nil {
		return nil, err
	}
	if err := readActionNotice(ctx, call, actor, "friend.request", fields{"request_id": in.RequestID}); err != nil {
		return nil, err
	}
	return fields{"request_id": in.RequestID, "status": status, "kind": kind}, notify(ctx, call, []int64{sender}, "friend.responded",
		fields{"request_id": in.RequestID, "user_id": actor, "status": status, "kind": kind})
}

func deleteFriend(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	actor, target, err := friendTarget(ctx, call, raw)
	if err != nil {
		return nil, err
	}
	var created time.Time
	err = call.DB.QueryRow(ctx, `DELETE FROM friendships WHERE user_low=$1 AND user_high=$2 RETURNING created_at`, min(actor, target), max(actor, target)).Scan(&created)
	if errors.Is(err, pgx.ErrNoRows) {
		return fields{"changed": false}, nil
	}
	if err != nil {
		return nil, err
	}
	if err := clearFriendDeletion(ctx, call.DB, actor, target); err != nil {
		return nil, err
	}
	var id int64
	err = call.DB.QueryRow(ctx, `WITH notices AS (
		INSERT INTO notifications(user_id,kind,data,created_at)
		SELECT v.user_id,'friend.deleted',jsonb_build_object('user_id',v.peer_id,'deleted_by',$1::bigint),statement_timestamp()
		FROM (VALUES ($1::bigint,$2::bigint),($2,$1)) AS v(user_id,peer_id) RETURNING id,user_id
	) SELECT id FROM notices WHERE user_id=$1`, actor, target).Scan(&id)
	if err != nil {
		return nil, err
	}
	blocked, err := pairBlocked(ctx, call.DB, actor, target)
	if err != nil {
		return nil, err
	}
	if !blocked {
		if _, err := call.DB.Exec(ctx, `INSERT INTO friend_deletions(notification_id,user_low,user_high,deleted_by,friendship_created_at,original_time_verified)
			VALUES($1,$2,$3,$4,$5,true)`, id, min(actor, target), max(actor, target), actor, created); err != nil {
			return nil, err
		}
	}
	if err := friendsChanged(call, actor, target); err != nil {
		return nil, err
	}
	return fields{"changed": true, "deletion_id": id}, call.Emit("notices.changed", []int64{actor, target}, fields{})
}

func invalidateFriendUndo(ctx context.Context, db rpc.DB, first, second int64) error {
	_, err := db.Exec(ctx, `UPDATE friend_deletions SET undo_allowed=false WHERE user_low=$1 AND user_high=$2 AND undo_allowed`, min(first, second), max(first, second))
	return err
}

func clearFriendDeletion(ctx context.Context, db rpc.DB, first, second int64) error {
	_, err := db.Exec(ctx, `DELETE FROM friend_deletions WHERE user_low=$1 AND user_high=$2`, min(first, second), max(first, second))
	return err
}

func undoFriendDeletion(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		userInput
		DeletionID int64 `json:"deletion_id"`
	}](raw)
	if err != nil {
		return nil, err
	}
	actor, target := call.Actor.ID, in.UserID
	if err := otherUser(actor, target); err != nil {
		return nil, err
	}
	if in.DeletionID <= 0 {
		return nil, rpc.Invalid("deletion_id must be positive")
	}
	if err := lockFriendPair(ctx, call.DB, actor, target); err != nil {
		return nil, err
	}
	unavailable := rpc.Conflict("This friendship deletion cannot be undone")
	var created time.Time
	err = call.DB.QueryRow(ctx, `DELETE FROM friend_deletions d USING notifications n
		WHERE d.notification_id=$1 AND d.user_low=$2 AND d.user_high=$3 AND d.deleted_by=$4 AND d.undo_allowed
		AND n.id=d.notification_id AND n.user_id=d.deleted_by AND n.kind='friend.deleted'
		AND `+recentDeletionNoticeSQL+` AND d.friendship_created_at<=n.created_at
		RETURNING d.friendship_created_at`, in.DeletionID, min(actor, target), max(actor, target), actor).Scan(&created)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, unavailable
	}
	if err != nil {
		return nil, err
	}
	if err := activeAccount(ctx, call.DB, target); err != nil {
		return nil, err
	}
	blocked, err := pairBlocked(ctx, call.DB, actor, target)
	if err != nil {
		return nil, err
	}
	if blocked {
		return nil, unavailable
	}
	// Any error rolls back consumption of the undo record along with this insert.
	tag, err := call.DB.Exec(ctx, `INSERT INTO friendships(user_low,user_high,created_at)
		SELECT $1,$2,$3 WHERE NOT EXISTS(SELECT 1 FROM friend_requests r WHERE `+pendingFriendPairSQL+`)
		ON CONFLICT DO NOTHING`, min(actor, target), max(actor, target), created)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, unavailable
	}
	return fields{"changed": true}, friendsChanged(call, actor, target)
}

func blockFriend(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	actor, target, err := friendTarget(ctx, call, raw)
	if err != nil {
		return nil, err
	}
	if err := activeAccount(ctx, call.DB, target); err != nil {
		return nil, err
	}
	if err := lockStoredFriendship(ctx, call.DB, actor, target); err != nil {
		return nil, err
	}
	tag, err := call.DB.Exec(ctx, `INSERT INTO friend_preferences(owner_id,other_id,blocked) VALUES($1,$2,true)
		ON CONFLICT (owner_id,other_id) DO UPDATE SET blocked=true WHERE NOT friend_preferences.blocked`, actor, target)
	if err != nil || tag.RowsAffected() == 0 {
		return fields{"blocked": true}, err
	}
	if err := invalidateFriendUndo(ctx, call.DB, actor, target); err != nil {
		return nil, err
	}
	var requestID, recipient int64
	err = call.DB.QueryRow(ctx, `UPDATE friend_requests r SET status='cancelled',resolved_at=clock_timestamp()
		WHERE `+pendingFriendPairSQL+` RETURNING id,recipient_id`, actor, target).Scan(&requestID, &recipient)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if err := readActionNotice(ctx, call, recipient, "friend.request", fields{"request_id": requestID}); err != nil {
			return nil, err
		}
	}
	return fields{"blocked": true}, preferenceChanged(ctx, call, actor, target)
}

func unblockFriend(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	actor, target, err := friendTarget(ctx, call, raw)
	if err != nil {
		return nil, err
	}
	if err := lockStoredFriendship(ctx, call.DB, actor, target); err != nil {
		return nil, err
	}
	tag, err := call.DB.Exec(ctx, `UPDATE friend_preferences SET blocked=false WHERE owner_id=$1 AND other_id=$2 AND blocked`, actor, target)
	if err != nil || tag.RowsAffected() == 0 {
		return fields{"blocked": false}, err
	}
	if err := invalidateFriendUndo(ctx, call.DB, actor, target); err != nil {
		return nil, err
	}
	return fields{"blocked": false}, preferenceChanged(ctx, call, actor, target)
}

func remarkFriend(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		userInput
		Remark *string `json:"remark"`
	}](raw)
	if err != nil {
		return nil, err
	}
	actor, target := call.Actor.ID, in.UserID
	if err := otherUser(actor, target); err != nil {
		return nil, err
	}
	if in.Remark == nil {
		return nil, rpc.Invalid("remark is required")
	}
	remark, err := cleanText("remark", *in.Remark, 64, false)
	if err != nil {
		return nil, err
	}
	if err := lockFriendPair(ctx, call.DB, actor, target); err != nil {
		return nil, err
	}
	var friends bool
	if err := call.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM friendships WHERE user_low=$1 AND user_high=$2)`, min(actor, target), max(actor, target)).Scan(&friends); err != nil {
		return nil, err
	}
	blocked, err := pairBlocked(ctx, call.DB, actor, target)
	if err != nil {
		return nil, err
	}
	if !friends || blocked {
		return nil, rpc.NotFound("Friend not found")
	}
	tag, err := call.DB.Exec(ctx, `INSERT INTO friend_preferences(owner_id,other_id,remark) VALUES($1,$2,$3)
		ON CONFLICT (owner_id,other_id) DO UPDATE SET remark=EXCLUDED.remark WHERE friend_preferences.remark<>EXCLUDED.remark`, actor, target, remark)
	if err != nil || tag.RowsAffected() == 0 {
		return fields{"remark": remark}, err
	}
	return fields{"remark": remark}, call.Emit("friends.changed", []int64{actor}, fields{"user_id": target})
}

func friendTarget(ctx context.Context, call rpc.Call, raw json.RawMessage) (int64, int64, error) {
	in, err := rpc.Decode[userInput](raw)
	if err != nil {
		return 0, 0, err
	}
	actor, target := call.Actor.ID, in.UserID
	if err := otherUser(actor, target); err != nil {
		return 0, 0, err
	}
	return actor, target, lockFriendPair(ctx, call.DB, actor, target)
}

func friendsChanged(call rpc.Call, actor, target int64) error {
	if err := call.Emit("friends.changed", []int64{actor}, fields{"user_id": target}); err != nil {
		return err
	}
	return call.Emit("friends.changed", []int64{target}, fields{"user_id": actor})
}

func preferenceChanged(ctx context.Context, call rpc.Call, actor, target int64) error {
	var affectsTarget bool
	err := call.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM friendships WHERE user_low=$1 AND user_high=$2)
		AND NOT EXISTS(SELECT 1 FROM friend_preferences WHERE owner_id=$3 AND other_id=$4 AND blocked)`, min(actor, target), max(actor, target), target, actor).Scan(&affectsTarget)
	if err != nil {
		return err
	}
	if affectsTarget {
		return friendsChanged(call, actor, target)
	}
	return call.Emit("friends.changed", []int64{actor}, fields{"user_id": target})
}

type friendStateItem struct {
	UserID            int64  `json:"user_id"`
	IsFriend          bool   `json:"is_friend"`
	Remark            string `json:"remark"`
	BlockedByMe       bool   `json:"blocked_by_me"`
	IncomingRequestID *int64 `json:"incoming_request_id"`
	OutgoingRequestID *int64 `json:"outgoing_request_id"`
	CanRequest        bool   `json:"can_request"`
}

func friendState(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[userInput](raw)
	if err != nil {
		return nil, err
	}
	actor := call.Actor.ID
	if err := otherUser(actor, in.UserID); err != nil {
		return nil, err
	}
	item := friendStateItem{UserID: in.UserID}
	var recent int
	err = call.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM friendships f
		WHERE f.user_low=LEAST($1::bigint,$2::bigint) AND f.user_high=GREATEST($1::bigint,$2::bigint))
		AND NOT COALESCE(mine.blocked,false) AND NOT EXISTS(SELECT 1 FROM friend_preferences p
			WHERE p.owner_id=$2 AND p.other_id=$1 AND p.blocked),
		COALESCE(mine.remark,''),COALESCE(mine.blocked,false),
		CASE WHEN r.recipient_id=$1 THEN r.id END,CASE WHEN r.sender_id=$1 THEN r.id END,`+recentFriendRequestsSQL+`
		FROM accounts a LEFT JOIN friend_preferences mine ON mine.owner_id=$1 AND mine.other_id=a.id
		LEFT JOIN friend_requests r ON `+pendingFriendPairSQL+` WHERE a.id=$2 AND `+eligibleAccount, actor, in.UserID).
		Scan(&item.IsFriend, &item.Remark, &item.BlockedByMe, &item.IncomingRequestID, &item.OutgoingRequestID, &recent)
	// The hint uses only visible state. The write enforces both users' preferences.
	item.CanRequest = !item.IsFriend && !item.BlockedByMe && item.IncomingRequestID == nil && item.OutgoingRequestID == nil && recent < dailyFriendRequestLimit
	return item, err
}

type person struct {
	UserID   int64   `json:"user_id"`
	Username string  `json:"username"`
	Nickname string  `json:"nickname"`
	AvatarID *string `json:"avatar_id"`
}

type friendItem struct {
	person
	Remark string `json:"remark"`
}

func listFriends(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		pageInput
		Blocked bool `json:"blocked"`
	}](raw)
	if err != nil {
		return nil, err
	}
	limit, err := in.size()
	if err != nil {
		return nil, err
	}
	query := `WITH peers AS (
		SELECT user_high AS id FROM friendships WHERE user_low=$1
		UNION ALL SELECT user_low FROM friendships WHERE user_high=$1
	) SELECT a.id,a.username,a.nickname,a.avatar_id::text,COALESCE(p.remark,'')
		FROM peers JOIN accounts a ON a.id=peers.id
		LEFT JOIN friend_preferences p ON p.owner_id=$1 AND p.other_id=a.id
		LEFT JOIN friend_preferences reverse ON reverse.owner_id=a.id AND reverse.other_id=$1
		WHERE ($2::bigint=0 OR a.id<$2) AND NOT COALESCE(p.blocked,false) AND NOT COALESCE(reverse.blocked,false)
		AND ` + eligibleAccount + ` ORDER BY a.id DESC LIMIT $3`
	if in.Blocked {
		query = `SELECT a.id,a.username,a.nickname,a.avatar_id::text,p.remark FROM friend_preferences p
			JOIN accounts a ON a.id=p.other_id WHERE p.owner_id=$1 AND p.blocked AND ($2::bigint=0 OR a.id<$2)
			ORDER BY a.id DESC LIMIT $3`
	}
	items, err := collect(ctx, call.DB, query, func(row pgx.CollectableRow) (friendItem, error) {
		var item friendItem
		err := row.Scan(&item.UserID, &item.Username, &item.Nickname, &item.AvatarID, &item.Remark)
		return item, err
	}, call.Actor.ID, in.BeforeID, limit+1)
	return paginate(items, limit, func(item friendItem) int64 { return item.UserID }), err
}

type friendRequestItem struct {
	RequestID   int64      `json:"request_id"`
	SenderID    int64      `json:"sender_id"`
	RecipientID int64      `json:"recipient_id"`
	Peer        person     `json:"peer"`
	Message     string     `json:"message"`
	Kind        string     `json:"kind"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	ResolvedAt  *time.Time `json:"resolved_at"`
}

func listFriendRequests(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		pageInput
		Direction string `json:"direction"`
		Status    string `json:"status"`
	}](raw)
	if err != nil {
		return nil, err
	}
	limit, err := in.size()
	if err != nil {
		return nil, err
	}
	owner, peer := "r.recipient_id", "r.sender_id"
	switch in.Direction {
	case "", "incoming":
	case "outgoing":
		owner, peer = peer, owner
	default:
		return nil, rpc.Invalid("direction must be incoming or outgoing")
	}
	if in.Status == "" {
		in.Status = pending
	}
	switch in.Status {
	case pending, accepted, rejected, cancelled:
	default:
		return nil, rpc.Invalid("Invalid request status")
	}
	items, err := collect(ctx, call.DB, `SELECT r.id,r.sender_id,r.recipient_id,a.id,a.username,a.nickname,a.avatar_id::text,
		r.message,r.kind,r.status,r.created_at,r.resolved_at FROM friend_requests r JOIN accounts a ON a.id=`+peer+`
		WHERE `+owner+`=$1 AND r.status=$2 AND ($3::bigint=0 OR r.id<$3) ORDER BY r.id DESC LIMIT $4`, func(row pgx.CollectableRow) (friendRequestItem, error) {
		var item friendRequestItem
		err := row.Scan(&item.RequestID, &item.SenderID, &item.RecipientID, &item.Peer.UserID,
			&item.Peer.Username, &item.Peer.Nickname, &item.Peer.AvatarID, &item.Message, &item.Kind, &item.Status, &item.CreatedAt, &item.ResolvedAt)
		return item, err
	}, call.Actor.ID, in.Status, in.BeforeID, limit+1)
	return paginate(items, limit, func(item friendRequestItem) int64 { return item.RequestID }), err
}

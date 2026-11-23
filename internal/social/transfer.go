package social

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"csac/v2/internal/rpc"

	"github.com/jackc/pgx/v5"
)

func transferGroup(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[groupUserInput](raw)
	if err != nil {
		return nil, err
	}
	actor, target := call.Actor.ID, in.UserID
	if err := otherUser(actor, target); err != nil {
		return nil, err
	}
	group, role, err := lockWithRole(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if err := needRole(group.visibility, role, role == ownerRole && group.ownerID == actor); err != nil {
		return nil, err
	}
	targetRole, err := memberRoleAt(ctx, call.DB, group.id, target)
	if err != nil {
		return nil, err
	}
	if targetRole == "" {
		return nil, rpc.NotFound("Group member not found")
	}
	if err := activeAccount(ctx, call.DB, target); err != nil {
		return nil, err
	}
	var onCooldown bool
	if err := call.DB.QueryRow(ctx, `SELECT coalesce(owner_transfer_cooldown_until>statement_timestamp(),false)
		FROM conversations WHERE id=$1`, group.id).Scan(&onCooldown); err != nil {
		return nil, err
	}
	if onCooldown {
		return nil, rpc.Conflict("Ownership transfers have a 28-day cooldown")
	}
	var id int64
	err = call.DB.QueryRow(ctx, `INSERT INTO group_transfers(conversation_id,from_user_id,to_user_id) VALUES($1,$2,$3)
		ON CONFLICT DO NOTHING RETURNING id`, group.id, actor, target).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, rpc.Conflict("An ownership transfer is already pending")
	}
	if err != nil {
		return nil, err
	}
	return fields{"conversation_id": group.id, "transfer_id": id, "status": pending},
		notify(ctx, call, []int64{target}, "group.transfer_requested", fields{"conversation_id": group.id, "transfer_id": id, "user_id": actor, "title": group.title})
}

type transferInput struct {
	groupInput
	TransferID int64 `json:"transfer_id"`
}

func respondTransfer(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		transferInput
		Accept *bool `json:"accept"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.TransferID <= 0 || in.Accept == nil {
		return nil, rpc.Invalid("transfer_id and accept are required")
	}
	return resolveTransfer(ctx, call, in.transferInput, responseStatus(in.Accept))
}

func cancelTransfer(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[transferInput](raw)
	if err != nil {
		return nil, err
	}
	if in.TransferID <= 0 {
		return nil, rpc.Invalid("transfer_id must be positive")
	}
	return resolveTransfer(ctx, call, in, cancelled)
}

func resolveTransfer(ctx context.Context, call rpc.Call, in transferInput, outcome string) (any, error) {
	group, role, err := lockWithRole(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	byOwner := outcome == cancelled
	if byOwner {
		if err := needRole(group.visibility, role, role == ownerRole && group.ownerID == call.Actor.ID); err != nil {
			return nil, err
		}
	}
	var from, to int64
	var current string
	err = call.DB.QueryRow(ctx, `SELECT from_user_id,to_user_id,status FROM group_transfers WHERE id=$1 AND conversation_id=$2
		AND CASE WHEN $4::boolean THEN from_user_id=$3 ELSE to_user_id=$3 END`, in.TransferID, group.id, call.Actor.ID, byOwner).
		Scan(&from, &to, &current)
	if err != nil {
		return nil, err
	}
	if current != pending || group.ownerID != from {
		return nil, rpc.Conflict("Ownership transfer is no longer pending")
	}
	if outcome == accepted {
		if role != memberRole && role != adminRole {
			return nil, rpc.Conflict("The recipient must still be a group member")
		}
		if err := activeAccount(ctx, call.DB, to); err != nil {
			return nil, err
		}
		// The unique owner index requires the old owner to be demoted first.
		if _, err := call.DB.Exec(ctx, `UPDATE conversation_members SET role='member' WHERE conversation_id=$1 AND user_id=$2`, group.id, from); err != nil {
			return nil, err
		}
		if _, err := call.DB.Exec(ctx, `UPDATE conversation_members SET role='owner',muted_until=NULL WHERE conversation_id=$1 AND user_id=$2`, group.id, to); err != nil {
			return nil, err
		}
	}
	var resolvedAt time.Time
	if err := call.DB.QueryRow(ctx, `UPDATE group_transfers SET status=$2,resolved_at=clock_timestamp()
		WHERE id=$1 RETURNING resolved_at`, in.TransferID, outcome).Scan(&resolvedAt); err != nil {
		return nil, err
	}
	if outcome == accepted {
		if _, err := call.DB.Exec(ctx, `UPDATE conversations SET owner_id=$2,owner_transfer_cooldown_until=$3 WHERE id=$1`,
			group.id, to, resolvedAt.Add(28*24*time.Hour)); err != nil {
			return nil, err
		}
		if err := groupChanged(ctx, call, group.id); err != nil {
			return nil, err
		}
	}
	if err := readActionNotice(ctx, call, to, "group.transfer_requested", fields{"transfer_id": in.TransferID}); err != nil {
		return nil, err
	}
	recipient := from
	if byOwner {
		recipient = to
	}
	return fields{"conversation_id": group.id, "transfer_id": in.TransferID, "status": outcome},
		notify(ctx, call, []int64{recipient}, "group.transfer_responded", fields{"conversation_id": group.id, "transfer_id": in.TransferID, "status": outcome})
}

// Only one transfer can be pending. A zero target cancels it on disband.
func cancelPendingTransfer(ctx context.Context, call rpc.Call, groupID, target int64) error {
	var id, userID int64
	err := call.DB.QueryRow(ctx, `UPDATE group_transfers SET status='cancelled',resolved_at=clock_timestamp()
		WHERE conversation_id=$1 AND status='pending' AND ($2::bigint=0 OR to_user_id=$2)
		RETURNING id,to_user_id`, groupID, target).Scan(&id, &userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return readActionNotice(ctx, call, userID, "group.transfer_requested", fields{"transfer_id": id})
}

type transferItem struct {
	TransferID     int64     `json:"transfer_id"`
	ConversationID int64     `json:"conversation_id"`
	GroupTitle     string    `json:"group_title"`
	FromUserID     int64     `json:"from_user_id"`
	ToUserID       int64     `json:"to_user_id"`
	Status         string    `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
}

func listTransfers(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[pageInput](raw)
	if err != nil {
		return nil, err
	}
	limit, err := in.size()
	if err != nil {
		return nil, err
	}
	items, err := collect(ctx, call.DB, `SELECT t.id,c.id,c.title,t.from_user_id,t.to_user_id,t.status,t.created_at FROM group_transfers t
		JOIN conversations c ON c.id=t.conversation_id JOIN conversation_members m ON m.conversation_id=c.id AND m.user_id=$1
		WHERE (t.from_user_id=$1 OR t.to_user_id=$1) AND t.status='pending' AND `+openGroupSQL+`
		AND ($2::bigint=0 OR t.id<$2) ORDER BY t.id DESC LIMIT $3`, pgx.RowToStructByPos[transferItem], call.Actor.ID, in.BeforeID, limit+1)
	return paginate(items, limit, func(item transferItem) int64 { return item.TransferID }), err
}

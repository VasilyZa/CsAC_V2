package social

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"time"

	"csac/v2/internal/moderation"
	"csac/v2/internal/rpc"

	"github.com/jackc/pgx/v5"
)

func joinGroup(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		groupInput
		Code   string `json:"code"`
		Answer string `json:"answer"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if len(in.Code) > maxGroupCode {
		return nil, rpc.Invalid("code must be at most 128 bytes")
	}
	answer, err := cleanText("answer", in.Answer, 500, false)
	if err != nil {
		return nil, err
	}
	group, role, err := lockWithRole(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	joined := fields{"conversation_id": group.id, "status": "joined"}
	if role != "" {
		return joined, nil
	}
	code := group.activeCode()
	validCode := group.policy == codePolicy && code != nil && in.Code != "" &&
		subtle.ConstantTimeCompare([]byte(*code), []byte(in.Code)) == 1
	if group.visibility == privateGroup && !validCode {
		return nil, rpc.NotFound("Group not found")
	}
	if err := requireAdmissionOpen(ctx, call.DB, group.id); err != nil {
		return nil, err
	}

	switch group.policy {
	case codePolicy, openPolicy:
		if group.policy == codePolicy && !validCode {
			return nil, rpc.Denied("A valid join code is required")
		}
		if err := admitMember(ctx, call, group.id, call.Actor.ID); err != nil {
			return nil, err
		}
		if group.policy == codePolicy && group.codeSingleUse {
			if _, err := rotateGroupCode(ctx, call.DB, group.id); err != nil {
				return nil, err
			}
		}
		return joined, finishAdmission(ctx, call, group.id, call.Actor.ID)
	case approvalPolicy:
		if group.joinQuestion != "" && answer == "" {
			return nil, rpc.Invalid("answer is required for this group")
		}
		if err := checkCapacity(ctx, call.DB, group.id); err != nil {
			return nil, err
		}
		id, err := newJoinRequest(ctx, call.DB, group.id, call.Actor.ID, nil, "application", answer)
		if err != nil {
			return nil, err
		}
		managers, err := groupRecipients(ctx, call.DB, group.id, true, 0)
		if err != nil {
			return nil, err
		}
		return fields{"conversation_id": group.id, "request_id": id, "status": pending},
			call.Emit("groups.join_requests_changed", managers, fields{"conversation_id": group.id})
	default:
		return nil, rpc.Denied("This group requires an invitation")
	}
}

// Admission callers already hold the conversation lock. Read expiry after
// acquiring it so a queued command uses the committed policy and current time.
func requireAdmissionOpen(ctx context.Context, db rpc.DB, groupID int64) error {
	var suspended bool
	if err := db.QueryRow(ctx, `SELECT coalesce(site_ban_until>statement_timestamp(),false)
		FROM conversations WHERE id=$1`, groupID).Scan(&suspended); err != nil {
		return err
	}
	if suspended {
		return moderation.GroupBanned()
	}
	return nil
}

func checkCapacity(ctx context.Context, db rpc.DB, groupID int64) error {
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM conversation_members WHERE conversation_id=$1`, groupID).Scan(&count); err != nil {
		return err
	}
	if count >= groupCap {
		return rpc.Conflict("Group member limit reached")
	}
	return nil
}

func admitMember(ctx context.Context, call rpc.Call, groupID, userID int64) error {
	if err := checkCapacity(ctx, call.DB, groupID); err != nil {
		return err
	}
	// The conversation lock keeps chat writes from advancing the head mid-admission.
	_, err := call.DB.Exec(ctx, `INSERT INTO conversation_members(conversation_id,user_id,read_seq,level)
		VALUES ($1,$2,(SELECT head FROM conversation_heads WHERE conversation_id=$1),
		group_level_from_points(coalesce((SELECT experience FROM group_member_experience WHERE conversation_id=$1 AND user_id=$2),0)))`, groupID, userID)
	return err
}

func finishAdmission(ctx context.Context, call rpc.Call, groupID, userID int64) error {
	if err := cancelJoinRequests(ctx, call, groupID, `j.user_id=$2`, userID); err != nil {
		return err
	}
	return memberChanged(ctx, call, groupID, userID)
}

// The caller holds the group lock and supplies an internal SQL predicate.
func cancelJoinRequests(ctx context.Context, call rpc.Call, groupID int64, filter string, args ...any) error {
	users, err := collect(ctx, call.DB, `WITH stopped AS (
		UPDATE group_join_requests j SET status='cancelled',resolved_at=clock_timestamp()
		WHERE j.conversation_id=$1 AND j.status='pending' AND (`+filter+`)
		RETURNING j.id,j.user_id,j.kind
	), marked AS (
		UPDATE notifications n SET read_at=clock_timestamp() FROM stopped s
		WHERE s.kind='invitation' AND n.user_id=s.user_id AND n.kind='group.invited'
		AND n.read_at IS NULL AND n.data->'request_id'=to_jsonb(s.id)
	)
	SELECT DISTINCT user_id FROM stopped WHERE kind='invitation'`, pgx.RowTo[int64], append([]any{groupID}, args...)...)
	if err != nil || len(users) == 0 {
		return err
	}
	return call.Emit("notices.changed", users, fields{})
}

func newJoinRequest(ctx context.Context, db rpc.DB, groupID, userID int64, inviterID *int64, kind, answer string) (int64, error) {
	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM group_join_requests WHERE conversation_id=$1 AND kind=$2 AND status='pending'`, groupID, kind).Scan(&count); err != nil {
		return 0, err
	}
	if count >= 500 {
		return 0, rpc.Conflict("Group pending request limit reached")
	}
	var id int64
	err := db.QueryRow(ctx, `INSERT INTO group_join_requests(conversation_id,user_id,inviter_id,kind,answer) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT DO NOTHING RETURNING id`, groupID, userID, inviterID, kind, answer).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = rpc.Conflict("A group request is already pending")
	}
	return id, err
}

func inviteGroup(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[groupUserInput](raw)
	if err != nil {
		return nil, err
	}
	actor, target := call.Actor.ID, in.UserID
	if err := otherUser(actor, target); err != nil {
		return nil, err
	}
	group, authority, err := lockForManagement(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if err := needRole(group.visibility, authority.role, authority.manages() || (authority.role == memberRole && group.allowInvite)); err != nil {
		return nil, err
	}
	if err := requireAdmissionOpen(ctx, call.DB, group.id); err != nil {
		return nil, err
	}
	currentRole, err := memberRoleAt(ctx, call.DB, group.id, target)
	if err != nil {
		return nil, err
	}
	if currentRole != "" {
		return nil, rpc.Conflict("User is already a group member")
	}
	if err := requireUnblockedInvitation(ctx, call.DB, actor, target); err != nil {
		return nil, err
	}
	var autoJoin bool
	err = call.DB.QueryRow(ctx, `SELECT a.allow_auto_join FROM accounts a WHERE a.id=$1 AND `+eligibleAccount+` FOR SHARE`, target).Scan(&autoJoin)
	if err != nil {
		return nil, err
	}
	if !autoJoin {
		if err := checkCapacity(ctx, call.DB, group.id); err != nil {
			return nil, err
		}
	}
	requestID, err := newJoinRequest(ctx, call.DB, group.id, target, &actor, "invitation", "")
	if err != nil {
		return nil, err
	}
	status, noticeKind := pending, "group.invited"
	if autoJoin {
		if err := admitMember(ctx, call, group.id, target); err != nil {
			return nil, err
		}
		if err := resolveJoinRequest(ctx, call.DB, requestID, accepted); err != nil {
			return nil, err
		}
		if err := finishAdmission(ctx, call, group.id, target); err != nil {
			return nil, err
		}
		status, noticeKind = "joined", "group.joined"
	}
	return fields{"conversation_id": group.id, "request_id": requestID, "status": status},
		notify(ctx, call, []int64{target}, noticeKind, fields{"conversation_id": group.id, "request_id": requestID, "user_id": actor, "title": group.title})
}

func requireUnblockedInvitation(ctx context.Context, db rpc.DB, inviter, target int64) error {
	if err := lockFriendPair(ctx, db, inviter, target); err != nil {
		return err
	}
	blocked, err := pairBlocked(ctx, db, inviter, target)
	if err != nil {
		return err
	}
	if blocked {
		return rpc.Denied("Invitation unavailable")
	}
	return nil
}

func resolveJoinRequest(ctx context.Context, db rpc.DB, requestID int64, status string) error {
	_, err := db.Exec(ctx, `UPDATE group_join_requests SET status=$2,resolved_at=clock_timestamp() WHERE id=$1`, requestID, status)
	return err
}

func respondJoin(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		groupInput
		RequestID int64 `json:"request_id"`
		Accept    *bool `json:"accept"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.RequestID <= 0 || in.Accept == nil {
		return nil, rpc.Invalid("request_id and accept are required")
	}
	group, authority, err := lockForManagement(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	var userID int64
	var inviter *int64
	var kind, current string
	err = call.DB.QueryRow(ctx, `SELECT user_id,inviter_id,kind,status FROM group_join_requests
		WHERE id=$1 AND conversation_id=$2 AND (kind='application' OR user_id=$3)`, in.RequestID, group.id, call.Actor.ID).
		Scan(&userID, &inviter, &kind, &current)
	if err != nil {
		return nil, err
	}
	if kind == "application" {
		if err := needRole(group.visibility, authority.role, authority.manages()); err != nil {
			return nil, err
		}
	}
	if current != pending {
		return nil, rpc.Conflict("Group request has already been resolved")
	}
	status := responseStatus(in.Accept)
	if status == accepted {
		if err := requireAdmissionOpen(ctx, call.DB, group.id); err != nil {
			return nil, err
		}
		if kind == "application" && (group.visibility != publicGroup || group.policy != approvalPolicy) {
			return nil, rpc.Conflict("This group no longer accepts applications")
		}
		if inviter != nil {
			if err := validateInviter(ctx, call, group, *inviter, userID); err != nil {
				return nil, err
			}
		}
		if err := activeAccount(ctx, call.DB, userID); err != nil {
			return nil, err
		}
		if err := admitMember(ctx, call, group.id, userID); err != nil {
			return nil, err
		}
	}
	if err := resolveJoinRequest(ctx, call.DB, in.RequestID, status); err != nil {
		return nil, err
	}
	if status == accepted {
		if err := finishAdmission(ctx, call, group.id, userID); err != nil {
			return nil, err
		}
	}

	recipient, noticeKind := userID, "group.join_responded"
	if inviter != nil {
		if err := readActionNotice(ctx, call, userID, "group.invited", fields{"request_id": in.RequestID}); err != nil {
			return nil, err
		}
		recipient, noticeKind = *inviter, "group.invitation_responded"
	} else {
		managers, err := groupRecipients(ctx, call.DB, group.id, true, 0)
		if err != nil {
			return nil, err
		}
		if err := call.Emit("groups.join_requests_changed", managers, fields{"conversation_id": group.id}); err != nil {
			return nil, err
		}
	}
	return fields{"conversation_id": group.id, "request_id": in.RequestID, "status": status},
		notify(ctx, call, []int64{recipient}, noticeKind, fields{"conversation_id": group.id, "request_id": in.RequestID, "user_id": call.Actor.ID, "status": status})
}

func validateInviter(ctx context.Context, call rpc.Call, group lockedGroup, inviter, target int64) error {
	role, err := memberRoleAt(ctx, call.DB, group.id, inviter)
	if err != nil {
		return err
	}
	if !moderator(role) && (role != memberRole || !group.allowInvite) {
		siteAdmin, err := moderation.IsSiteAdmin(ctx, call.DB, inviter, false)
		if err != nil {
			return err
		}
		if !siteAdmin {
			return rpc.Conflict("Invitation is no longer valid")
		}
	}
	if err := activeAccount(ctx, call.DB, inviter); err != nil {
		return err
	}
	return requireUnblockedInvitation(ctx, call.DB, inviter, target)
}

type joinRequestItem struct {
	RequestID      int64  `json:"request_id"`
	ConversationID int64  `json:"conversation_id"`
	GroupTitle     string `json:"group_title"`
	person
	InviterID *int64    `json:"inviter_id"`
	Kind      string    `json:"kind"`
	Answer    string    `json:"answer,omitempty"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func scanJoinRequest(row pgx.CollectableRow) (joinRequestItem, error) {
	var item joinRequestItem
	err := row.Scan(&item.RequestID, &item.ConversationID, &item.GroupTitle, &item.UserID, &item.Username, &item.Nickname,
		&item.AvatarID, &item.InviterID, &item.Kind, &item.Answer, &item.Status, &item.CreatedAt)
	return item, err
}

const joinRequestColumns = `j.id,j.conversation_id,c.title,a.id,a.username,a.nickname,a.avatar_id::text,
	j.inviter_id,j.kind,j.answer,j.status,j.created_at`

func listApplications(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		groupInput
		pageInput
	}](raw)
	if err != nil {
		return nil, err
	}
	limit, err := in.size()
	if err != nil {
		return nil, err
	}
	visibility, authority, err := readGroupAuthority(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if err := needRole(visibility, authority.role, authority.manages()); err != nil {
		return nil, err
	}
	items, err := collect(ctx, call.DB, `SELECT `+joinRequestColumns+` FROM group_join_requests j
		JOIN conversations c ON c.id=j.conversation_id JOIN accounts a ON a.id=j.user_id
		LEFT JOIN conversation_members actor ON actor.conversation_id=c.id AND actor.user_id=$2 AND actor.role IN ('owner','admin')
		WHERE j.conversation_id=$1 AND `+openGroupSQL+` AND j.kind='application' AND j.status='pending'
		AND (actor.user_id IS NOT NULL OR EXISTS(SELECT 1 FROM active_site_admins WHERE user_id=$2))
		AND ($3::bigint=0 OR j.id<$3) ORDER BY j.id DESC LIMIT $4`, scanJoinRequest, in.ConversationID, call.Actor.ID, in.BeforeID, limit+1)
	return paginate(items, limit, func(item joinRequestItem) int64 { return item.RequestID }), err
}

func listInvitations(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[pageInput](raw)
	if err != nil {
		return nil, err
	}
	limit, err := in.size()
	if err != nil {
		return nil, err
	}
	items, err := collect(ctx, call.DB, `SELECT `+joinRequestColumns+` FROM group_join_requests j
		JOIN conversations c ON c.id=j.conversation_id JOIN accounts a ON a.id=j.user_id
		WHERE j.user_id=$1 AND `+openGroupSQL+` AND j.kind='invitation' AND j.status='pending'
		AND ($2::bigint=0 OR j.id<$2) ORDER BY j.id DESC LIMIT $3`, scanJoinRequest, call.Actor.ID, in.BeforeID, limit+1)
	return paginate(items, limit, func(item joinRequestItem) int64 { return item.RequestID }), err
}

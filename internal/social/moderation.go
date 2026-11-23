package social

import (
	"context"
	"encoding/json"
	"time"

	"csac/v2/internal/rpc"
)

func setGroupAdmin(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		groupUserInput
		Admin *bool `json:"admin"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if err := otherUser(call.Actor.ID, in.UserID); err != nil {
		return nil, err
	}
	if in.Admin == nil {
		return nil, rpc.Invalid("admin is required")
	}
	group, authority, err := lockForManagement(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if err := needRole(group.visibility, authority.role, authority.administers()); err != nil {
		return nil, err
	}
	target, err := memberRoleAt(ctx, call.DB, group.id, in.UserID)
	if err != nil {
		return nil, err
	}
	if target == "" {
		return nil, rpc.NotFound("Group member not found")
	}
	if target == ownerRole {
		return nil, rpc.Denied("The owner's role cannot be changed here")
	}
	newRole := memberRole
	if *in.Admin {
		if err := activeAccount(ctx, call.DB, in.UserID); err != nil {
			return nil, err
		}
		newRole = adminRole
	}
	if newRole != target {
		if _, err := call.DB.Exec(ctx, `UPDATE conversation_members SET role=$3 WHERE conversation_id=$1 AND user_id=$2`, group.id, in.UserID, newRole); err != nil {
			return nil, err
		}
		if newRole == memberRole && !group.allowInvite {
			if err := cancelJoinRequests(ctx, call, group.id, `j.inviter_id=$2`, in.UserID); err != nil {
				return nil, err
			}
		}
		if err := memberChanged(ctx, call, group.id, in.UserID); err != nil {
			return nil, err
		}
	}
	return fields{"role": newRole}, nil
}

func moderationTarget(ctx context.Context, call rpc.Call, groupID, targetID int64, ownerSelfTitle bool) (lockedGroup, error) {
	if targetID <= 0 || (!ownerSelfTitle && targetID == call.Actor.ID) {
		return lockedGroup{}, rpc.Invalid("user_id must identify a valid moderation target")
	}
	group, authority, err := lockForManagement(ctx, call, groupID)
	if err != nil {
		return group, err
	}
	if err := needRole(group.visibility, authority.role, authority.manages()); err != nil {
		return group, err
	}
	targetRole, err := memberRoleAt(ctx, call.DB, group.id, targetID)
	if err != nil {
		return group, err
	}
	if targetRole == "" {
		return group, rpc.NotFound("Group member not found")
	}
	canEditOwnerTitle := ownerSelfTitle && (targetID == call.Actor.ID || authority.siteAdmin)
	if (targetRole == ownerRole && !canEditOwnerTitle) || (targetRole == adminRole && !authority.administers()) {
		return group, rpc.Denied("You cannot moderate this member")
	}
	return group, nil
}

func setMemberTitle(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		groupUserInput
		Title *string `json:"title"`
		Level *int16  `json:"level"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.Title == nil && in.Level == nil {
		return nil, rpc.Invalid("Supply title or level")
	}
	if in.Title != nil {
		value, err := cleanText("title", *in.Title, 16, false)
		if err != nil {
			return nil, err
		}
		in.Title = &value
	}
	if in.Level != nil && (*in.Level < 1 || *in.Level > 100) {
		return nil, rpc.Invalid("level must be between 1 and 100")
	}
	group, err := moderationTarget(ctx, call, in.ConversationID, in.UserID, true)
	if err != nil {
		return nil, err
	}
	var title string
	var level int16
	err = call.DB.QueryRow(ctx, `UPDATE conversation_members SET title=COALESCE($3,title),level=COALESCE($4,level),
		level_custom=level_custom OR $4::smallint IS NOT NULL
		WHERE conversation_id=$1 AND user_id=$2 RETURNING coalesce(nullif(title,''),group_default_title(level)),level`, group.id, in.UserID, in.Title, in.Level).Scan(&title, &level)
	if err != nil {
		return nil, err
	}
	return fields{"title": title, "level": level}, memberChanged(ctx, call, group.id, in.UserID)
}

func muteMember(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		groupUserInput
		Minutes *int32 `json:"minutes"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.Minutes == nil || *in.Minutes < 0 || *in.Minutes > 43200 {
		return nil, rpc.Invalid("minutes must be between 0 and 43200")
	}
	group, err := moderationTarget(ctx, call, in.ConversationID, in.UserID, false)
	if err != nil {
		return nil, err
	}
	var expires *time.Time
	err = call.DB.QueryRow(ctx, `UPDATE conversation_members
		SET muted_until=CASE WHEN $3::integer=0 THEN NULL ELSE clock_timestamp()+make_interval(mins=>$3) END
		WHERE conversation_id=$1 AND user_id=$2 RETURNING muted_until`, group.id, in.UserID, *in.Minutes).Scan(&expires)
	if err != nil {
		return nil, err
	}
	return fields{"muted_until": expires}, memberChanged(ctx, call, group.id, in.UserID)
}

func kickMember(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[groupUserInput](raw)
	if err != nil {
		return nil, err
	}
	group, err := moderationTarget(ctx, call, in.ConversationID, in.UserID, false)
	if err != nil {
		return nil, err
	}
	if err := removeMember(ctx, call, group.id, in.UserID); err != nil {
		return nil, err
	}
	if group.policy == codePolicy && !group.usesFixedCode() {
		if _, err := rotateGroupCode(ctx, call.DB, group.id); err != nil {
			return nil, err
		}
	}
	return fields{"conversation_id": group.id}, notify(ctx, call, []int64{in.UserID}, "group.kicked", fields{"conversation_id": group.id, "title": group.title})
}

func leaveGroup(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := decodeGroupInput(raw)
	if err != nil {
		return nil, err
	}
	group, role, err := lockWithRole(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if err := needRole(group.visibility, role, role != ""); err != nil {
		return nil, err
	}
	if role == ownerRole {
		return nil, rpc.Conflict("Transfer ownership or disband the group before leaving")
	}
	return fields{"conversation_id": group.id}, removeMember(ctx, call, group.id, call.Actor.ID)
}

func removeMember(ctx context.Context, call rpc.Call, groupID, userID int64) error {
	if _, err := call.DB.Exec(ctx, `DELETE FROM conversation_members WHERE conversation_id=$1 AND user_id=$2`, groupID, userID); err != nil {
		return err
	}
	if err := cancelJoinRequests(ctx, call, groupID, `j.inviter_id=$2 OR j.user_id=$2`, userID); err != nil {
		return err
	}
	if err := cancelPendingTransfer(ctx, call, groupID, userID); err != nil {
		return err
	}
	return memberChanged(ctx, call, groupID, userID)
}

func disbandGroup(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := decodeGroupInput(raw)
	if err != nil {
		return nil, err
	}
	group, authority, err := lockForManagement(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if err := needRole(group.visibility, authority.role, authority.administers()); err != nil {
		return nil, err
	}
	users, err := groupRecipients(ctx, call.DB, group.id, false, 0)
	if err != nil {
		return nil, err
	}
	if _, err := call.DB.Exec(ctx, `UPDATE conversations SET archived_at=clock_timestamp(),invite_code=NULL WHERE id=$1`, group.id); err != nil {
		return nil, err
	}
	if err := cancelJoinRequests(ctx, call, group.id, `true`); err != nil {
		return nil, err
	}
	if err := cancelPendingTransfer(ctx, call, group.id, 0); err != nil {
		return nil, err
	}
	if err := call.Emit("groups.changed", users, fields{"conversation_id": group.id}); err != nil {
		return nil, err
	}
	return fields{"conversation_id": group.id}, notify(ctx, call, users, "group.disbanded", fields{"conversation_id": group.id, "title": group.title})
}

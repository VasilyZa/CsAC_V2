package social

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"csac/v2/internal/media"
	"csac/v2/internal/rpc"

	"github.com/jackc/pgx/v5"
)

const (
	groupCap       = 5000
	ownerRole      = "owner"
	adminRole      = "admin"
	memberRole     = "member"
	publicGroup    = "public"
	privateGroup   = "private"
	openPolicy     = "open"
	codePolicy     = "code"
	approvalPolicy = "approval"
	invitePolicy   = "invite"
	maxGroupCode   = 128
)

type groupInput struct {
	ConversationID int64 `json:"conversation_id"`
}

type groupUserInput struct {
	groupInput
	userInput
}

type lockedGroup struct {
	id            int64
	ownerID       int64
	title         string
	visibility    string
	policy        string
	code          *string
	fixedCode     *string
	allowInvite   bool
	codeSingleUse bool
	joinQuestion  string
}

type groupCodeInput struct {
	CodeSingleUse *bool   `json:"code_single_use"`
	FixedCode     *string `json:"fixed_code"`
}

func (g lockedGroup) usesFixedCode() bool {
	return g.policy == codePolicy && !g.codeSingleUse && g.fixedCode != nil
}

func (g lockedGroup) activeCode() *string {
	if g.usesFixedCode() {
		return g.fixedCode
	}
	return g.code
}

func (g *lockedGroup) configureCode(in groupCodeInput) error {
	if in.CodeSingleUse != nil {
		g.codeSingleUse = *in.CodeSingleUse
	}
	if in.FixedCode != nil {
		value := *in.FixedCode
		if value == "" {
			g.fixedCode = nil
		} else {
			if len(value) > maxGroupCode || strings.TrimSpace(value) == "" || strings.IndexByte(value, 0) >= 0 {
				return rpc.Invalid("fixed_code must be nonblank and at most 128 bytes")
			}
			if g.policy != codePolicy || (in.CodeSingleUse != nil && *in.CodeSingleUse) {
				return rpc.Invalid("fixed_code requires reusable code admission")
			}
			g.fixedCode = in.FixedCode
			g.codeSingleUse = false
		}
	}
	if g.policy != codePolicy || g.usesFixedCode() {
		g.code = nil
	} else if g.code == nil || *g.code == "" {
		value := rand.Text()
		g.code = &value
	}
	return nil
}

// Order all group policy and membership writes against each other and chat's
// conversation share lock. Read roles and dependent rows after taking the lock.
func lockGroup(ctx context.Context, db rpc.DB, id int64) (lockedGroup, error) {
	var group lockedGroup
	if id <= 0 {
		return group, rpc.Invalid("conversation_id must be positive")
	}
	err := db.QueryRow(ctx, `SELECT id,owner_id,title,visibility,join_policy,invite_code,fixed_code,allow_invite,code_single_use,join_question
		FROM conversations WHERE id=$1 AND kind='group' AND archived_at IS NULL FOR UPDATE`, id).
		Scan(&group.id, &group.ownerID, &group.title, &group.visibility, &group.policy, &group.code, &group.fixedCode, &group.allowInvite, &group.codeSingleUse, &group.joinQuestion)
	return group, err
}

func memberRoleAt(ctx context.Context, db rpc.DB, conversationID, userID int64) (string, error) {
	var role string
	err := db.QueryRow(ctx, `SELECT role FROM conversation_members WHERE conversation_id=$1 AND user_id=$2`, conversationID, userID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return role, err
}

func moderator(role string) bool {
	return role == ownerRole || role == adminRole
}

func needRole(visibility, role string, allowed bool) error {
	if allowed {
		return nil
	}
	if visibility == privateGroup && role == "" {
		return rpc.NotFound("Group not found")
	}
	return rpc.Denied("Your group role does not allow this operation")
}

func lockWithRole(ctx context.Context, call rpc.Call, id int64) (lockedGroup, string, error) {
	group, err := lockGroup(ctx, call.DB, id)
	if err != nil {
		return group, "", err
	}
	role, err := memberRoleAt(ctx, call.DB, id, call.Actor.ID)
	return group, role, err
}

func readGroupRole(ctx context.Context, call rpc.Call, id int64) (visibility, role string, err error) {
	if id <= 0 {
		return "", "", rpc.Invalid("conversation_id must be positive")
	}
	err = call.DB.QueryRow(ctx, `SELECT c.visibility,COALESCE(m.role,'') FROM conversations c
		LEFT JOIN conversation_members m ON m.conversation_id=c.id AND m.user_id=$2
		WHERE c.id=$1 AND c.kind='group' AND c.archived_at IS NULL`, id, call.Actor.ID).Scan(&visibility, &role)
	return
}

func groupRecipients(ctx context.Context, db rpc.DB, groupID int64, managersOnly bool, affectedUser int64) ([]int64, error) {
	return collect(ctx, db, `SELECT a.id FROM conversation_members m JOIN accounts a ON a.id=m.user_id
		WHERE m.conversation_id=$1 AND (NOT $2::boolean OR m.role IN ('owner','admin')) AND `+eligibleAccount+`
		UNION SELECT a.id FROM accounts a WHERE a.id=$3 AND `+eligibleAccount,
		pgx.RowTo[int64], groupID, managersOnly, affectedUser)
}

func groupChanged(ctx context.Context, call rpc.Call, groupID int64) error {
	users, err := groupRecipients(ctx, call.DB, groupID, false, 0)
	if err != nil {
		return err
	}
	return call.Emit("groups.changed", users, fields{"conversation_id": groupID})
}

func memberChanged(ctx context.Context, call rpc.Call, groupID, userID int64) error {
	users, err := groupRecipients(ctx, call.DB, groupID, false, userID)
	if err != nil {
		return err
	}
	return call.Emit("groups.members_changed", users, fields{"conversation_id": groupID, "user_id": userID})
}

type groupInfoInput struct {
	Title        *string `json:"title"`
	Description  *string `json:"description"`
	Announcement *string `json:"announcement"`
	AvatarID     *string `json:"avatar_id"`
}

func (input *groupInfoInput) clean(create bool) error {
	if create && input.Title == nil {
		return rpc.Invalid("title is required")
	}
	if !create && input.Title == nil && input.Description == nil && input.Announcement == nil && input.AvatarID == nil {
		return rpc.Invalid("Supply at least one group profile field")
	}
	for _, field := range []struct {
		name     string
		value    *string
		limit    int
		required bool
	}{
		{"title", input.Title, 32, true},
		{"description", input.Description, 2000, false},
		{"announcement", input.Announcement, 4000, false},
	} {
		if field.value == nil {
			continue
		}
		value, err := cleanText(field.name, *field.value, field.limit, field.required)
		if err != nil {
			return err
		}
		*field.value = value
	}
	return nil
}

func ownedAvatar(ctx context.Context, call rpc.Call, value *string) (*string, error) {
	if value == nil || *value == "" {
		return nil, nil
	}
	if !media.ValidID(*value) {
		return nil, rpc.Invalid("avatar_id must be a UUID or an empty string")
	}
	var found bool
	err := call.DB.QueryRow(ctx, `SELECT true FROM media_objects WHERE id=$1::uuid AND owner_id=$2
		AND state='ready' AND purpose='avatar' AND mime LIKE 'image/%' AND purged_at IS NULL
		AND (expires_at IS NULL OR expires_at>clock_timestamp()) FOR SHARE`, *value, call.Actor.ID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, rpc.NotFound("Usable avatar not found")
	}
	return value, err
}

func checkVisibility(value string) error {
	if value != privateGroup && value != publicGroup {
		return rpc.Invalid("visibility must be private or public")
	}
	return nil
}

func checkPolicy(value string) error {
	switch value {
	case openPolicy, codePolicy, approvalPolicy, invitePolicy:
		return nil
	default:
		return rpc.Invalid("join_policy must be open, code, approval, or invite")
	}
}

func createGroup(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		groupInfoInput
		groupCodeInput
		Visibility   string `json:"visibility"`
		JoinPolicy   string `json:"join_policy"`
		Discoverable *bool  `json:"discoverable"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if err := in.clean(true); err != nil {
		return nil, err
	}
	if in.Visibility == "" {
		in.Visibility = privateGroup
	}
	if in.JoinPolicy == "" {
		in.JoinPolicy = approvalPolicy
	}
	if err := checkVisibility(in.Visibility); err != nil {
		return nil, err
	}
	if err := checkPolicy(in.JoinPolicy); err != nil {
		return nil, err
	}
	group := lockedGroup{policy: in.JoinPolicy, codeSingleUse: true}
	if err := group.configureCode(in.groupCodeInput); err != nil {
		return nil, err
	}
	avatarID, err := ownedAvatar(ctx, call, in.AvatarID)
	if err != nil {
		return nil, err
	}
	var groupID int64
	// This transaction owns the newly inserted row before the membership and head exist.
	err = call.DB.QueryRow(ctx, `INSERT INTO conversations(kind,title,description,announcement,avatar_id,owner_id,visibility,join_policy,
		invite_code,fixed_code,code_single_use,discoverable)
		VALUES('group',$1,COALESCE($2,''),COALESCE($3,''),$4,$5,$6,$7,$8,$9,$10,COALESCE($11,true)) RETURNING id`,
		*in.Title, in.Description, in.Announcement, avatarID, call.Actor.ID, in.Visibility, group.policy,
		group.code, group.fixedCode, group.codeSingleUse, in.Discoverable).Scan(&groupID)
	if err != nil {
		return nil, err
	}
	if _, err := call.DB.Exec(ctx, `INSERT INTO conversation_members(conversation_id,user_id,role) VALUES($1,$2,'owner')`, groupID, call.Actor.ID); err != nil {
		return nil, err
	}
	if _, err := call.DB.Exec(ctx, `INSERT INTO conversation_heads(conversation_id) VALUES($1)`, groupID); err != nil {
		return nil, err
	}
	return fields{"conversation_id": groupID}, call.Emit("groups.changed", []int64{call.Actor.ID}, fields{"conversation_id": groupID})
}

func editGroup(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		groupInput
		groupInfoInput
	}](raw)
	if err != nil {
		return nil, err
	}
	if err := in.clean(false); err != nil {
		return nil, err
	}
	group, role, err := lockWithRole(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if err := needRole(group.visibility, role, moderator(role)); err != nil {
		return nil, err
	}
	avatarID, err := ownedAvatar(ctx, call, in.AvatarID)
	if err != nil {
		return nil, err
	}
	if _, err := call.DB.Exec(ctx, `UPDATE conversations SET title=COALESCE($2,title),description=COALESCE($3,description),
		announcement=COALESCE($4,announcement),avatar_id=CASE WHEN $5::boolean THEN $6::uuid ELSE avatar_id END WHERE id=$1`,
		group.id, in.Title, in.Description, in.Announcement, in.AvatarID != nil, avatarID); err != nil {
		return nil, err
	}
	return fields{"conversation_id": group.id}, groupChanged(ctx, call, group.id)
}

func groupSettings(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		groupInput
		groupCodeInput
		Visibility   *string `json:"visibility"`
		JoinPolicy   *string `json:"join_policy"`
		Discoverable *bool   `json:"discoverable"`
		AllowInvite  *bool   `json:"allow_invite"`
		JoinQuestion *string `json:"join_question"`
		GlobalMute   *bool   `json:"global_mute"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.Visibility == nil && in.JoinPolicy == nil && in.Discoverable == nil && in.AllowInvite == nil &&
		in.CodeSingleUse == nil && in.FixedCode == nil && in.JoinQuestion == nil && in.GlobalMute == nil {
		return nil, rpc.Invalid("Supply at least one group setting")
	}
	group, role, err := lockWithRole(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if err := needRole(group.visibility, role, moderator(role)); err != nil {
		return nil, err
	}
	if in.Visibility != nil {
		if err := checkVisibility(*in.Visibility); err != nil {
			return nil, err
		}
		group.visibility = *in.Visibility
	}
	if in.JoinPolicy != nil {
		if err := checkPolicy(*in.JoinPolicy); err != nil {
			return nil, err
		}
		group.policy = *in.JoinPolicy
	}
	if in.JoinQuestion != nil {
		group.joinQuestion, err = cleanText("join_question", *in.JoinQuestion, 200, false)
		if err != nil {
			return nil, err
		}
	}
	if err := group.configureCode(in.groupCodeInput); err != nil {
		return nil, err
	}
	if _, err := call.DB.Exec(ctx, `UPDATE conversations SET visibility=$2,join_policy=$3,invite_code=$4,fixed_code=$5,
		allow_invite=COALESCE($6,allow_invite),code_single_use=$7,join_question=$8,
		global_mute=COALESCE($9,global_mute),discoverable=COALESCE($10,discoverable) WHERE id=$1`, group.id, group.visibility, group.policy,
		group.code, group.fixedCode, in.AllowInvite, group.codeSingleUse, group.joinQuestion, in.GlobalMute, in.Discoverable); err != nil {
		return nil, err
	}
	if group.visibility != publicGroup || group.policy != approvalPolicy {
		if err := cancelJoinRequests(ctx, call, group.id, `j.kind='application'`); err != nil {
			return nil, err
		}
	}
	if in.AllowInvite != nil && !*in.AllowInvite {
		if err := cancelJoinRequests(ctx, call, group.id, `j.kind='invitation' AND NOT EXISTS (
			SELECT 1 FROM conversation_members m WHERE m.conversation_id=j.conversation_id AND m.user_id=j.inviter_id AND m.role IN ('owner','admin'))`); err != nil {
			return nil, err
		}
	}
	return fields{"conversation_id": group.id}, groupChanged(ctx, call, group.id)
}

func rotateGroupCode(ctx context.Context, db rpc.DB, groupID int64) (string, error) {
	code := rand.Text()
	_, err := db.Exec(ctx, `UPDATE conversations SET invite_code=$2 WHERE id=$1`, groupID, code)
	return code, err
}

func resetGroupCode(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := decodeGroupInput(raw)
	if err != nil {
		return nil, err
	}
	group, role, err := lockWithRole(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if err := needRole(group.visibility, role, role == ownerRole); err != nil {
		return nil, err
	}
	if group.policy != codePolicy {
		return nil, rpc.Conflict("The group does not use code admission")
	}
	if group.usesFixedCode() {
		return nil, rpc.Conflict("Set fixed_code to replace the current code")
	}
	code, err := rotateGroupCode(ctx, call.DB, group.id)
	if err != nil {
		return nil, err
	}
	return fields{"conversation_id": group.id, "invite_code": code}, groupChanged(ctx, call, group.id)
}

type groupSummary struct {
	ConversationID int64      `json:"conversation_id"`
	Title          string     `json:"title"`
	Description    string     `json:"description"`
	AvatarID       *string    `json:"avatar_id"`
	OwnerID        int64      `json:"owner_id"`
	OwnerNickname  string     `json:"owner_nickname"`
	Visibility     string     `json:"visibility"`
	JoinPolicy     string     `json:"join_policy"`
	MemberCount    int64      `json:"member_count"`
	SiteBanUntil   *time.Time `json:"site_ban_until,omitempty"`
	SiteBanReason  *string    `json:"site_ban_reason,omitempty"`
}

const groupSummaryColumns = `c.id,c.title,c.description,c.avatar_id::text,c.owner_id,o.nickname,c.visibility,c.join_policy,
	(SELECT count(*) FROM conversation_members m WHERE m.conversation_id=c.id),
	CASE WHEN c.site_ban_until>statement_timestamp() THEN c.site_ban_until END,
	CASE WHEN c.site_ban_until>statement_timestamp() THEN c.site_ban_reason END`
const openGroupSQL = `c.kind='group' AND c.archived_at IS NULL`
const listedGroupSQL = `c.visibility='public' AND c.discoverable`

func (g *groupSummary) columns() []any {
	return []any{&g.ConversationID, &g.Title, &g.Description, &g.AvatarID, &g.OwnerID, &g.OwnerNickname, &g.Visibility, &g.JoinPolicy, &g.MemberCount, &g.SiteBanUntil, &g.SiteBanReason}
}

func groupPage(ctx context.Context, call rpc.Call, page pageInput, limit int, filter string, extra ...any) (any, error) {
	items, err := collect(ctx, call.DB, `SELECT `+groupSummaryColumns+` FROM conversations c JOIN accounts o ON o.id=c.owner_id
		WHERE `+openGroupSQL+` AND ($1::bigint=0 OR c.id<$1) AND `+filter+` ORDER BY c.id DESC LIMIT $2`,
		func(row pgx.CollectableRow) (groupSummary, error) {
			var group groupSummary
			err := row.Scan(group.columns()...)
			return group, err
		}, append([]any{page.BeforeID, limit + 1}, extra...)...)
	return paginate(items, limit, func(item groupSummary) int64 { return item.ConversationID }), err
}

func discoverGroups(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		pageInput
		Query string `json:"query"`
	}](raw)
	if err != nil {
		return nil, err
	}
	limit, err := in.size()
	if err != nil {
		return nil, err
	}
	query, err := cleanText("query", in.Query, 80, false)
	if err != nil {
		return nil, err
	}
	return groupPage(ctx, call, in.pageInput, limit, listedGroupSQL+` AND ($3='' OR strpos(lower(c.title),lower($3))>0)`, query)
}

const myGroupSQL = `EXISTS(SELECT 1 FROM conversation_members m WHERE m.conversation_id=c.id AND m.user_id=$3)`

func listGroups(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[pageInput](raw)
	if err != nil {
		return nil, err
	}
	limit, err := in.size()
	if err != nil {
		return nil, err
	}
	return groupPage(ctx, call, in, limit, myGroupSQL, call.Actor.ID)
}

func ownedGroups(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		pageInput
		OwnerID *int64 `json:"owner_id"`
	}](raw)
	if err != nil {
		return nil, err
	}
	limit, err := in.size()
	if err != nil {
		return nil, err
	}
	ownerID := call.Actor.ID
	if in.OwnerID != nil {
		if *in.OwnerID <= 0 {
			return nil, rpc.Invalid("owner_id must be positive")
		}
		ownerID = *in.OwnerID
	}
	return groupPage(ctx, call, in.pageInput, limit, `c.owner_id=$3 AND ($3=$4 OR (`+listedGroupSQL+`))`, ownerID, call.Actor.ID)
}

func commonGroups(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		pageInput
		userInput
	}](raw)
	if err != nil {
		return nil, err
	}
	if err := otherUser(call.Actor.ID, in.UserID); err != nil {
		return nil, err
	}
	limit, err := in.size()
	if err != nil {
		return nil, err
	}
	return groupPage(ctx, call, in.pageInput, limit, myGroupSQL+` AND EXISTS(SELECT 1 FROM conversation_members m
		JOIN accounts a ON a.id=m.user_id WHERE m.conversation_id=c.id AND m.user_id=$4 AND `+eligibleAccount+`)`, call.Actor.ID, in.UserID)
}

type groupProfileItem struct {
	groupSummary
	Role             *string `json:"role"`
	Announcement     *string `json:"announcement,omitempty"`
	InviteCode       *string `json:"invite_code,omitempty"`
	AllowInvite      *bool   `json:"allow_invite,omitempty"`
	Discoverable     *bool   `json:"discoverable,omitempty"`
	CodeSingleUse    *bool   `json:"code_single_use,omitempty"`
	JoinQuestion     *string `json:"join_question,omitempty"`
	GlobalMute       *bool   `json:"global_mute,omitempty"`
	PendingRequestID *int64  `json:"pending_request_id"`
}

func groupProfile(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := decodeGroupInput(raw)
	if err != nil {
		return nil, err
	}
	var item groupProfileItem
	err = call.DB.QueryRow(ctx, `SELECT `+groupSummaryColumns+`,m.role,
		CASE WHEN m.role IS NOT NULL THEN c.announcement END,
		CASE WHEN m.role IN ('owner','admin') AND c.join_policy='code' THEN
			CASE WHEN c.code_single_use THEN c.invite_code ELSE COALESCE(c.fixed_code,c.invite_code) END END,
		CASE WHEN m.role IS NOT NULL THEN c.allow_invite END,
		CASE WHEN m.role IN ('owner','admin') THEN c.discoverable END,
		CASE WHEN m.role IN ('owner','admin') AND c.join_policy='code' THEN c.code_single_use END,
		CASE WHEN c.join_policy='approval' THEN c.join_question END,
		CASE WHEN m.role IS NOT NULL THEN c.global_mute END,
		(SELECT j.id FROM group_join_requests j WHERE j.conversation_id=c.id AND j.user_id=$2 AND j.kind='application' AND j.status='pending')
		FROM conversations c JOIN accounts o ON o.id=c.owner_id
		LEFT JOIN conversation_members m ON m.conversation_id=c.id AND m.user_id=$2
		WHERE c.id=$1 AND `+openGroupSQL+` AND (c.visibility='public' OR m.role IS NOT NULL)`, in.ConversationID, call.Actor.ID).
		Scan(append(item.columns(), &item.Role, &item.Announcement, &item.InviteCode, &item.AllowInvite, &item.Discoverable, &item.CodeSingleUse, &item.JoinQuestion, &item.GlobalMute, &item.PendingRequestID)...)
	return item, err
}

type memberItem struct {
	person
	Role       string     `json:"role"`
	Title      string     `json:"title"`
	Level      int16      `json:"level"`
	JoinedAt   time.Time  `json:"joined_at"`
	MutedUntil *time.Time `json:"muted_until"`
	Muted      bool       `json:"muted"`
}

func listGroupMembers(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		groupInput
		AfterUserID int64 `json:"after_user_id"`
		Limit       int   `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.AfterUserID < 0 {
		return nil, rpc.Invalid("after_user_id must be positive when supplied")
	}
	limit, err := pageSize(in.Limit)
	if err != nil {
		return nil, err
	}
	visibility, role, err := readGroupRole(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if err := needRole(visibility, role, role != ""); err != nil {
		return nil, err
	}
	// Recheck access in the data statement in case membership changed after the first read.
	items, err := collect(ctx, call.DB, `SELECT a.id,a.username,a.nickname,a.avatar_id::text,m.role,coalesce(nullif(m.title,''),group_default_title(m.level)),m.level,m.joined_at,m.muted_until,
		COALESCE(m.muted_until>clock_timestamp(),false) FROM conversation_members m JOIN accounts a ON a.id=m.user_id
		JOIN conversations c ON c.id=m.conversation_id
		JOIN conversation_members actor ON actor.conversation_id=c.id AND actor.user_id=$2
		WHERE c.id=$1 AND `+openGroupSQL+` AND m.user_id>$3 AND `+eligibleAccount+` ORDER BY m.user_id LIMIT $4`,
		func(row pgx.CollectableRow) (memberItem, error) {
			var item memberItem
			err := row.Scan(&item.UserID, &item.Username, &item.Nickname, &item.AvatarID, &item.Role, &item.Title, &item.Level, &item.JoinedAt, &item.MutedUntil, &item.Muted)
			return item, err
		}, in.ConversationID, call.Actor.ID, in.AfterUserID, limit+1)
	if err != nil {
		return nil, err
	}
	var next *int64
	if len(items) > limit {
		id := items[limit-1].UserID
		next = &id
		items = items[:limit]
	}
	return fields{"items": items, "next_after_user_id": next}, nil
}

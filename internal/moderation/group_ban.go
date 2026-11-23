package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"csac/v2/internal/rpc"
	"github.com/jackc/pgx/v5"
)

type groupBanRef struct {
	ConversationID int64 `json:"conversation_id"`
}

type groupBanState struct {
	groupBanRef
	Banned bool       `json:"banned"`
	Until  *time.Time `json:"ban_until"`
	Reason string     `json:"reason,omitempty"`
}

// GroupBanned is returned after the caller's access to the group is established.
func GroupBanned() error {
	return rpc.Error("group_banned", "This group is suspended by a site administrator", http.StatusForbidden)
}

func banGroup(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		groupBanRef
		Days   int    `json:"ban_days"`
		Reason string `json:"reason"`
	}](raw)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.Reason)
	if in.ConversationID <= 0 || in.Days < 1 || in.Days > 36500 || reason == "" ||
		utf8.RuneCountInString(reason) > 500 || strings.ContainsFunc(reason, unicode.IsControl) {
		return nil, rpc.Invalid("Supply a group, a ban duration of 1 to 36500 days, and a reason of 1 to 500 characters")
	}
	if err := requireBanAdmin(ctx, call, true); err != nil {
		return nil, err
	}
	// The conversation lock also orders joins, owner changes, sends and typing.
	var owner int64
	var title string
	out := groupBanState{groupBanRef: in.groupBanRef, Banned: true, Reason: reason}
	err = call.DB.QueryRow(ctx, `UPDATE conversations
		SET site_ban_until=clock_timestamp()+$2::integer*interval '24 hours', site_ban_reason=$3
		WHERE id=$1 AND kind='group' AND archived_at IS NULL
		RETURNING owner_id,title,site_ban_until`, in.ConversationID, in.Days, reason).Scan(&owner, &title, &out.Until)
	if err != nil {
		return nil, err
	}
	return out, notifyGroupOwner(ctx, call, owner, title, "group.banned", out)
}

func unbanGroup(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[groupBanRef](raw)
	if err != nil {
		return nil, err
	}
	if in.ConversationID <= 0 {
		return nil, rpc.Invalid("conversation_id must be positive")
	}
	if err := requireBanAdmin(ctx, call, true); err != nil {
		return nil, err
	}
	var owner int64
	var title string
	var previous *time.Time
	err = call.DB.QueryRow(ctx, `SELECT owner_id,title,site_ban_until FROM conversations
		WHERE id=$1 AND kind='group' AND archived_at IS NULL FOR UPDATE`, in.ConversationID).Scan(&owner, &title, &previous)
	if err != nil {
		return nil, err
	}
	out := groupBanState{groupBanRef: in}
	if previous == nil {
		return out, nil
	}
	if _, err := call.DB.Exec(ctx, `UPDATE conversations SET site_ban_until=NULL,site_ban_reason='' WHERE id=$1`, in.ConversationID); err != nil {
		return nil, err
	}
	return out, notifyGroupOwner(ctx, call, owner, title, "group.unbanned", out)
}

func notifyGroupOwner(ctx context.Context, call rpc.Call, ownerID int64, title, kind string, state groupBanState) error {
	payload := struct {
		groupBanState
		Title string `json:"title"`
	}{state, title}
	if _, err := call.DB.Exec(ctx, `INSERT INTO notifications(user_id,kind,data) VALUES ($1,$2,$3::jsonb)`, ownerID, kind, payload); err != nil {
		return err
	}
	return call.Emit("notices.changed", []int64{ownerID}, struct{}{})
}

// Recheck persisted administrator access at execution time. Hold the account
// share lock throughout a write so role revocation and active bans are ordered.
func requireBanAdmin(ctx context.Context, call rpc.Call, lock bool) error {
	query := `SELECT true FROM accounts WHERE id=$1 AND site_admin
		AND (status='active' OR (status='banned' AND ban_until<=statement_timestamp()))`
	if lock {
		query += ` FOR SHARE`
	}
	var allowed bool
	err := call.DB.QueryRow(ctx, query, call.Actor.ID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return rpc.Denied("Administrator access required")
	}
	return err
}

type banItem struct {
	ID            int64      `json:"id"`
	Title         string     `json:"title"`
	Username      string     `json:"username,omitempty"`
	OwnerID       *int64     `json:"owner_id,omitempty"`
	OwnerNickname string     `json:"owner_nickname,omitempty"`
	Until         *time.Time `json:"ban_until"`
	Reason        string     `json:"reason"`
}

func listBans(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		Kind     string `json:"kind"`
		BeforeID int64  `json:"before_id"`
		Limit    int    `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.Limit == 0 {
		in.Limit = 30
	}
	if in.Limit < 1 || in.Limit > 100 || in.BeforeID < 0 || (in.Kind != "" && in.Kind != "group" && in.Kind != "account") {
		return nil, rpc.Invalid("Invalid ban page")
	}
	if err := requireBanAdmin(ctx, call, false); err != nil {
		return nil, err
	}
	query := `SELECT c.id,c.title,''::text,c.owner_id,a.nickname,c.site_ban_until,c.site_ban_reason
		FROM conversations c JOIN accounts a ON a.id=c.owner_id
		WHERE c.kind='group' AND c.archived_at IS NULL AND c.site_ban_until>statement_timestamp()
		AND ($1::bigint=0 OR c.id<$1) ORDER BY c.id DESC LIMIT $2`
	if in.Kind == "account" {
		query = `SELECT id,nickname,username,NULL::bigint,''::text,ban_until,ban_reason FROM accounts
			WHERE status='banned' AND (ban_until IS NULL OR ban_until>statement_timestamp())
			AND ($1::bigint=0 OR id<$1) ORDER BY id DESC LIMIT $2`
	}
	rows, err := call.DB.Query(ctx, query, in.BeforeID, in.Limit+1)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[banItem])
	if err != nil {
		return nil, err
	}
	out := struct {
		Items        []banItem `json:"items"`
		NextBeforeID int64     `json:"next_before_id,omitempty"`
	}{Items: items}
	if len(items) > in.Limit {
		out.Items = items[:in.Limit]
		out.NextBeforeID = out.Items[in.Limit-1].ID
	}
	return out, nil
}

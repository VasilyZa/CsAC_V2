// Package social owns friends, group membership and moderation, and notices.
package social

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"csac/v2/internal/rpc"

	"github.com/jackc/pgx/v5"
)

// Register adds the social commands to the shared transactional dispatcher.
func Register(registry *rpc.Registry) {
	write := func(name string, handle rpc.Handler) {
		registry.Register(name, rpc.Method{Access: rpc.Member, Write: true, Dedup: true, Handle: handle})
	}
	read := func(name string, handle rpc.Handler) {
		registry.Register(name, rpc.Method{Access: rpc.Member, Handle: handle})
	}
	read("friends.list", listFriends)
	read("friends.requests", listFriendRequests)
	read("friends.state", friendState)
	read("friends.common_groups", commonGroups)
	write("friends.request", requestFriend)
	write("friends.recover", recoverFriend)
	write("friends.respond", respondFriend)
	write("friends.delete", deleteFriend)
	write("friends.undo_delete", undoFriendDeletion)
	write("friends.block", blockFriend)
	write("friends.unblock", unblockFriend)
	write("friends.remark", remarkFriend)

	read("groups.discover", discoverGroups)
	read("groups.list", listGroups)
	read("groups.owned", ownedGroups)
	read("groups.profile", groupProfile)
	read("groups.members", listGroupMembers)
	read("groups.activity", groupActivity)
	read("groups.join_requests", listApplications)
	read("groups.invitations", listInvitations)
	read("groups.transfer_requests", listTransfers)
	write("groups.create", createGroup)
	write("groups.join", joinGroup)
	write("groups.invite", inviteGroup)
	write("groups.respond_join", respondJoin)
	write("groups.edit", editGroup)
	write("groups.settings", groupSettings)
	write("groups.reset_code", resetGroupCode)
	write("groups.set_admin", setGroupAdmin)
	write("groups.set_title", setMemberTitle)
	write("groups.mute", muteMember)
	write("groups.kick", kickMember)
	write("groups.leave", leaveGroup)
	write("groups.transfer", transferGroup)
	write("groups.respond_transfer", respondTransfer)
	write("groups.cancel_transfer", cancelTransfer)
	write("groups.disband", disbandGroup)

	registry.Register("notices.list", rpc.Method{Access: rpc.SignedIn, Handle: listNotices})
	registry.Register("notices.count", rpc.Method{Access: rpc.SignedIn, Handle: countNotices})
	registry.Register("notices.read", rpc.Method{Access: rpc.SignedIn, Write: true, Dedup: true, Handle: readNotices})
}

const (
	pending   = "pending"
	accepted  = "accepted"
	rejected  = "rejected"
	cancelled = "cancelled"

	// Match account authentication during the interval between ban expiration
	// and maintenance. The dispatcher applies the configured email policy.
	eligibleAccount = `(a.status='active' OR (a.status='banned' AND a.ban_until<=clock_timestamp()))`
)

type fields = map[string]any

type userInput struct {
	UserID int64 `json:"user_id"`
}

func otherUser(myID, userID int64) error {
	if userID <= 0 || userID == myID {
		return rpc.Invalid("user_id must identify another user")
	}
	return nil
}

func activeAccount(ctx context.Context, db rpc.DB, userID int64) error {
	var present bool
	err := db.QueryRow(ctx, `SELECT true FROM accounts a WHERE a.id=$1 AND `+eligibleAccount, userID).Scan(&present)
	if errors.Is(err, pgx.ErrNoRows) {
		return rpc.NotFound("User not found")
	}
	return err
}

func cleanText(field, value string, max int, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if strings.ContainsRune(value, 0) || !utf8.ValidString(value) || utf8.RuneCountInString(value) > max {
		return "", rpc.Invalid(fmt.Sprintf("%s must be valid text of at most %d characters", field, max))
	}
	if required && value == "" {
		return "", rpc.Invalid(field + " is required")
	}
	return value, nil
}

type pageInput struct {
	BeforeID int64 `json:"before_id"`
	Limit    int   `json:"limit"`
}

func (p pageInput) size() (int, error) {
	if p.BeforeID < 0 {
		return 0, rpc.Invalid("before_id must be positive when supplied")
	}
	return pageSize(p.Limit)
}

func pageSize(limit int) (int, error) {
	if limit == 0 {
		return 50, nil
	}
	if limit < 1 || limit > 100 {
		return 0, rpc.Invalid("limit must be between 1 and 100")
	}
	return limit, nil
}

type resultPage[T any] struct {
	Items        []T    `json:"items"`
	NextBeforeID *int64 `json:"next_before_id"`
}

func paginate[T any](items []T, limit int, id func(T) int64) resultPage[T] {
	page := resultPage[T]{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		next := id(items[limit-1])
		page.NextBeforeID = &next
	}
	return page
}

func collect[T any](ctx context.Context, db rpc.DB, sql string, scan pgx.RowToFunc[T], args ...any) ([]T, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scan)
}

func decodeGroupInput(raw json.RawMessage) (groupInput, error) {
	in, err := rpc.Decode[groupInput](raw)
	if err == nil && in.ConversationID <= 0 {
		err = rpc.Invalid("conversation_id must be positive")
	}
	return in, err
}

func responseStatus(accept *bool) string {
	if *accept {
		return accepted
	}
	return rejected
}

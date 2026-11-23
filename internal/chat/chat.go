// Package chat owns conversation timelines, read state, and message commands.
package chat

import (
	"context"
	"encoding/json"
	"errors"

	"csac/v2/internal/moderation"
	"csac/v2/internal/realtime"
	"csac/v2/internal/rpc"
	"github.com/jackc/pgx/v5"
)

type service struct {
	live   *realtime.Hub
	typers *typingWindows
}

func Register(r *rpc.Registry, live *realtime.Hub) {
	s := service{live: live, typers: newTypingWindows()}
	write := func(name string, f rpc.Handler) {
		r.Register(name, rpc.Method{Access: rpc.Member, Write: true, Dedup: true, Handle: f})
	}
	read := func(name string, f rpc.Handler) { r.Register(name, rpc.Method{Access: rpc.Member, Handle: f}) }
	write("chat.direct.open", openDirect)
	write("chat.send", send)
	write("chat.read", markRead)
	write("chat.recall", recall)
	write("chat.feature.set", feature)
	write("conversations.hide", hide)
	read("chat.history", history)
	read("chat.message.get", getMessage)
	RegisterAttention(r)
	read("chat.feature.list", featured)
	read("chat.feature.stats", featureStats)
	read("conversations.list", conversations)
	read("stickers.list", listStickerCatalog)
	read("chat.presence", s.presence)
	// Typing uses a transaction to hold the conversation lock through fanout.
	r.Register("chat.typing", rpc.Method{Access: rpc.Member, Write: true, Handle: s.typing})
}

type conversationRef struct {
	ConversationID int64 `json:"conversation_id"`
}

type access struct {
	kind  string
	role  string
	muted bool
	peer  int64
}

func accessTo(ctx context.Context, db rpc.DB, id, user int64, lock bool) (access, error) {
	var a access
	var suspended bool
	sql := `SELECT c.kind,m.role,
		c.kind='group' AND (coalesce(m.muted_until>statement_timestamp(),false) OR (c.global_mute AND m.role='member')),
		CASE WHEN c.direct_low=$2 THEN c.direct_high ELSE coalesce(c.direct_low,0) END,
		coalesce(c.site_ban_until>statement_timestamp(),false)
		FROM conversation_members m JOIN conversations c ON c.id=m.conversation_id
		WHERE c.id=$1 AND m.user_id=$2 AND c.archived_at IS NULL`
	if lock {
		var lockedID int64
		err := db.QueryRow(ctx, `SELECT id FROM conversations WHERE id=$1 AND archived_at IS NULL FOR SHARE`, id).Scan(&lockedID)
		if errors.Is(err, pgx.ErrNoRows) {
			return a, rpc.NotFound("Conversation unavailable")
		}
		if err != nil {
			return a, err
		}
	}
	err := db.QueryRow(ctx, sql, id, user).Scan(&a.kind, &a.role, &a.muted, &a.peer, &suspended)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, rpc.NotFound("Conversation unavailable")
	}
	if err != nil {
		return a, err
	}
	if a.kind == "direct" {
		return a, requireFriend(ctx, db, user, a.peer, lock)
	}
	if suspended {
		return a, moderation.GroupBanned()
	}
	return a, nil
}

func requireFriend(ctx context.Context, db rpc.DB, a, b int64, lock bool) error {
	sql := `SELECT true FROM friendships f JOIN accounts p ON p.id=$2 AND (p.status='active' OR (p.status='banned' AND p.ban_until<=now()))
		WHERE f.user_low=least($1,$2) AND f.user_high=greatest($1,$2)
		AND NOT EXISTS (SELECT 1 FROM friend_preferences x WHERE x.blocked AND ((x.owner_id=$1 AND x.other_id=$2) OR (x.owner_id=$2 AND x.other_id=$1)))`
	if lock {
		// Read preferences in a fresh statement after any concurrent block
		// transaction releasing its friendship row lock has committed.
		var row int64
		err := db.QueryRow(ctx, `SELECT user_low FROM friendships WHERE user_low=$1 AND user_high=$2 FOR SHARE`, min(a, b), max(a, b)).Scan(&row)
		if errors.Is(err, pgx.ErrNoRows) {
			return rpc.Denied("Direct conversation requires an active friendship")
		}
		if err != nil {
			return err
		}
	}
	var ok bool
	if err := db.QueryRow(ctx, sql, a, b).Scan(&ok); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return rpc.Denied("Direct conversation requires an active friendship")
		}
		return err
	}
	return nil
}

func openDirect(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	input, err := rpc.Decode[struct {
		UserID int64 `json:"user_id"`
	}](raw)
	if err != nil {
		return nil, err
	}
	uid := call.Actor.ID
	if input.UserID <= 0 || input.UserID == uid {
		return nil, rpc.Invalid("Choose another user")
	}
	if err := requireFriend(ctx, call.DB, uid, input.UserID, true); err != nil {
		return nil, err
	}
	low, high := min(uid, input.UserID), max(uid, input.UserID)
	var id int64
	err = call.DB.QueryRow(ctx, `INSERT INTO conversations(kind,direct_low,direct_high) VALUES ('direct',$1,$2)
		ON CONFLICT (direct_low,direct_high) DO NOTHING RETURNING id`, low, high).Scan(&id)
	created := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		err = call.DB.QueryRow(ctx, `SELECT id FROM conversations WHERE direct_low=$1 AND direct_high=$2`, low, high).Scan(&id)
	}
	if err != nil {
		return nil, err
	}
	if created {
		if _, err := call.DB.Exec(ctx, `INSERT INTO conversation_members(conversation_id,user_id) VALUES ($1,$2),($1,$3)`, id, low, high); err != nil {
			return nil, err
		}
		if _, err := call.DB.Exec(ctx, `INSERT INTO conversation_heads(conversation_id) VALUES ($1)`, id); err != nil {
			return nil, err
		}
		if err := call.Emit("conversation.created", []int64{low, high}, conversationRef{id}); err != nil {
			return nil, err
		}
	}
	return conversationRef{id}, nil
}

func memberIDs(ctx context.Context, db rpc.DB, id int64) ([]int64, error) {
	rows, err := db.Query(ctx, `SELECT user_id FROM conversation_members WHERE conversation_id=$1 ORDER BY user_id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

func canManage(a access) bool { return a.kind == "group" && (a.role == "owner" || a.role == "admin") }

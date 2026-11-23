package chat

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"csac/v2/internal/rpc"
	"github.com/jackc/pgx/v5"
)

type summary struct {
	ID           int64     `json:"conversation_id"`
	Kind         string    `json:"kind"`
	Title        string    `json:"title"`
	Avatar       *string   `json:"avatar_id,omitempty"`
	Peer         *int64    `json:"peer_id,omitempty"`
	Head         int64     `json:"head,string"`
	Read         int64     `json:"read_through,string"`
	Unread       int64     `json:"unread"`
	LastActivity time.Time `json:"last_activity"`
	LastKind     string    `json:"last_kind,omitempty"`
	Preview      string    `json:"preview,omitempty"`
	Hidden       bool      `json:"hidden"`
}

func conversations(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		Limit         int        `json:"limit"`
		Before        *time.Time `json:"before_activity"`
		BeforeID      int64      `json:"before_id"`
		IncludeHidden bool       `json:"include_hidden"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.Limit == 0 {
		in.Limit = 50
	}
	if in.Limit < 1 || in.Limit > 100 || (in.Before != nil && in.BeforeID <= 0) {
		return nil, rpc.Invalid("Invalid conversation page")
	}
	rows, err := call.DB.Query(ctx, `WITH visible AS MATERIALIZED (
		SELECT c.id,c.kind,c.title,c.avatar_id,h.head,h.last_sent_at,p.id AS peer_id,p.nickname,p.avatar_id AS peer_avatar,
			coalesce(nullif(pref.remark,''),p.nickname) AS peer_title, m.read_seq,m.hidden,
			(c.site_ban_until IS NULL OR c.site_ban_until<=statement_timestamp()) AS history_visible
		FROM conversation_members m JOIN conversations c ON c.id=m.conversation_id JOIN conversation_heads h ON h.conversation_id=c.id
		LEFT JOIN accounts p ON p.id=CASE WHEN c.direct_low=$1 THEN c.direct_high ELSE c.direct_low END
			AND (p.status='active' OR (p.status='banned' AND p.ban_until<=statement_timestamp()))
		LEFT JOIN friend_preferences pref ON pref.owner_id=$1 AND pref.other_id=p.id
		WHERE m.user_id=$1 AND c.archived_at IS NULL AND ($2 OR NOT m.hidden)
		AND ($3::timestamptz IS NULL OR (h.last_sent_at,c.id)<($3,$4))
		AND (c.kind='group' OR (p.id IS NOT NULL AND NOT coalesce(pref.blocked,false)
			AND EXISTS(SELECT 1 FROM friendships f WHERE f.user_low=c.direct_low AND f.user_high=c.direct_high)
			AND NOT EXISTS(SELECT 1 FROM friend_preferences bp WHERE bp.owner_id=p.id AND bp.other_id=$1 AND bp.blocked)))
		ORDER BY h.last_sent_at DESC,c.id DESC LIMIT $5)
		SELECT v.id,v.kind,coalesce(v.peer_title,v.title),coalesce(v.peer_avatar,v.avatar_id)::text,v.peer_id,v.head,v.read_seq,
		CASE WHEN v.history_visible THEN (SELECT count(*) FROM messages incoming
			WHERE incoming.conversation_id=v.id AND incoming.sender_id<>$1 AND incoming.seq>v.read_seq AND incoming.kind<>'system') ELSE 0 END,
		v.last_sent_at,coalesce(last.kind,''),coalesce(CASE WHEN last.recalled_at IS NULL THEN left(last.body,200) END,''),v.hidden
		FROM visible v LEFT JOIN LATERAL (
			SELECT m.kind,m.recalled_at,m.body FROM messages m WHERE v.history_visible AND m.conversation_id=v.id AND m.kind<>'system'
			ORDER BY m.seq DESC LIMIT 1
		) last ON true
		ORDER BY v.last_sent_at DESC,v.id DESC`, call.Actor.ID, in.IncludeHidden, in.Before, in.BeforeID, in.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]summary, 0, in.Limit)
	more := false
	for rows.Next() {
		if len(out) == in.Limit {
			more = true
			break
		}
		var c summary
		if err := rows.Scan(&c.ID, &c.Kind, &c.Title, &c.Avatar, &c.Peer, &c.Head, &c.Read, &c.Unread, &c.LastActivity, &c.LastKind, &c.Preview, &c.Hidden); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return struct {
		Conversations []summary `json:"conversations"`
		More          bool      `json:"has_more"`
	}{out, more}, rows.Err()
}

func markRead(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[messageRef](raw)
	if err != nil {
		return nil, err
	}
	if in.ConversationID <= 0 || in.Seq < 0 {
		return nil, rpc.Invalid("Invalid read position")
	}
	if _, err = accessTo(ctx, call.DB, in.ConversationID, call.Actor.ID, true); err != nil {
		return nil, err
	}
	var head, previous int64
	err = call.DB.QueryRow(ctx, `SELECT h.head,m.read_seq FROM conversation_heads h JOIN conversation_members m USING(conversation_id)
		WHERE h.conversation_id=$1 AND m.user_id=$2 FOR UPDATE OF m`, in.ConversationID, call.Actor.ID).Scan(&head, &previous)
	if err != nil {
		return nil, err
	}
	if in.Seq > head {
		return nil, rpc.Invalid("Read position exceeds the conversation head")
	}
	if in.Seq <= previous {
		in.Seq = previous
		return in, nil
	}
	if _, err = call.DB.Exec(ctx, `UPDATE conversation_members SET read_seq=$3 WHERE conversation_id=$1 AND user_id=$2`, in.ConversationID, call.Actor.ID, in.Seq); err != nil {
		return nil, err
	}
	return in, call.Emit("conversation.read", []int64{call.Actor.ID}, in)
}

func hide(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		conversationRef
		Hidden bool `json:"hidden"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if _, err = accessTo(ctx, call.DB, in.ConversationID, call.Actor.ID, true); err != nil {
		return nil, err
	}
	if _, err = call.DB.Exec(ctx, `UPDATE conversation_members SET hidden=$3 WHERE conversation_id=$1 AND user_id=$2`, in.ConversationID, call.Actor.ID, in.Hidden); err != nil {
		return nil, err
	}
	return in, call.Emit("conversation.visibility", []int64{call.Actor.ID}, in)
}

func recall(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[messageRef](raw)
	if err != nil {
		return nil, err
	}
	if in.ConversationID <= 0 || in.Seq <= 0 {
		return nil, rpc.Invalid("Choose a conversation and message sequence")
	}
	a, err := reviewAccessTo(ctx, call.DB, in.ConversationID, call.Actor.ID, true)
	if err != nil {
		return nil, err
	}
	var recalled *time.Time
	var prior, allowed *string
	// Read the clock only after the row lock has been acquired.
	err = call.DB.QueryRow(ctx, `WITH locked AS MATERIALIZED (
			SELECT recalled_at,recall_kind,sender_id,kind,created_at FROM messages
			WHERE conversation_id=$1 AND seq=$2 FOR NO KEY UPDATE
		) SELECT recalled_at,recall_kind,chat_recall_kind(sender_id,$3,kind,sender_id=$6,$4,$5,created_at,clock_timestamp())
		FROM locked`, in.ConversationID, in.Seq, call.Actor.ID, a.role, a.siteAdmin, a.owner).
		Scan(&recalled, &prior, &allowed)
	if err != nil {
		return nil, err
	}
	if allowed == nil {
		return nil, rpc.Denied("Recall window closed or role not permitted")
	}
	out := struct {
		messageRef
		Kind       string    `json:"recall_kind"`
		RecalledAt time.Time `json:"recalled_at"`
	}{messageRef: in, Kind: *allowed}
	if recalled != nil {
		out.Kind, out.RecalledAt = "unknown", recalled.UTC()
		if prior != nil {
			out.Kind = *prior
		}
		return out, nil
	}
	if err = call.DB.QueryRow(ctx, `UPDATE messages SET recalled_at=clock_timestamp(),recalled_by=$3,recall_kind=$4
		WHERE conversation_id=$1 AND seq=$2 RETURNING recalled_at`, in.ConversationID, in.Seq, call.Actor.ID, out.Kind).Scan(&out.RecalledAt); err != nil {
		return nil, err
	}
	out.RecalledAt = out.RecalledAt.UTC()
	if _, err = call.DB.Exec(ctx, `DELETE FROM featured_messages WHERE conversation_id=$1 AND seq=$2`, in.ConversationID, in.Seq); err != nil {
		return nil, err
	}
	ids, err := memberIDs(ctx, call.DB, in.ConversationID)
	if err != nil {
		return nil, err
	}
	return out, call.EmitChat(in.ConversationID, 0, "message.recalled", ids, out)
}

type featureChange struct {
	messageRef
	Featured bool `json:"featured"`
}

func feature(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[featureChange](raw)
	if err != nil {
		return nil, err
	}
	if in.ConversationID <= 0 || in.Seq <= 0 {
		return nil, rpc.Invalid("Choose a conversation and message sequence")
	}
	a, err := reviewAccessTo(ctx, call.DB, in.ConversationID, call.Actor.ID, true)
	if err != nil {
		return nil, err
	}
	if !canManage(a) {
		return nil, rpc.Denied("Group manager role required")
	}
	var subjectID int64
	var subjectName string
	// Order feature changes and recalls of this message. The weaker row lock
	// still lets a concurrent chat.send reference the original as a reply.
	err = call.DB.QueryRow(ctx, `SELECT m.sender_id,a.nickname FROM messages m JOIN accounts a ON a.id=m.sender_id
		WHERE m.conversation_id=$1 AND m.seq=$2 AND m.recalled_at IS NULL AND m.kind<>'system' FOR NO KEY UPDATE OF m`, in.ConversationID, in.Seq).
		Scan(&subjectID, &subjectName)
	if err != nil {
		if !in.Featured && errors.Is(err, pgx.ErrNoRows) {
			return in, nil
		}
		return nil, err
	}
	var changed int64
	action := "feature_removed"
	if in.Featured {
		action = "feature_added"
		tag, err := call.DB.Exec(ctx, `INSERT INTO featured_messages(conversation_id,seq,added_by) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, in.ConversationID, in.Seq, call.Actor.ID)
		if err != nil {
			return nil, err
		}
		changed = tag.RowsAffected()
	} else {
		tag, err := call.DB.Exec(ctx, `DELETE FROM featured_messages WHERE conversation_id=$1 AND seq=$2`, in.ConversationID, in.Seq)
		if err != nil {
			return nil, err
		}
		changed = tag.RowsAffected()
	}
	if changed == 0 {
		return in, nil
	}
	ids, err := memberIDs(ctx, call.DB, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if err := call.EmitChat(in.ConversationID, 0, "message.featured", ids, in); err != nil {
		return nil, err
	}
	notice := message{
		messageCreated: messageCreated{messageRef: messageRef{ConversationID: in.ConversationID}, Sender: call.Actor.ID, Kind: "system"},
		SystemAction:   &action, SystemSubject: &subjectID, SystemSubjectName: &subjectName,
	}
	if err := call.DB.QueryRow(ctx, `UPDATE conversation_heads SET head=head+1 WHERE conversation_id=$1
		RETURNING head,clock_timestamp()`, in.ConversationID).Scan(&notice.Seq, &notice.At); err != nil {
		return nil, err
	}
	if _, err := call.DB.Exec(ctx, `INSERT INTO messages(conversation_id,seq,sender_id,kind,created_at,system_action,system_subject_id)
		VALUES ($1,$2,$3,'system',$4,$5,$6)`, in.ConversationID, notice.Seq, notice.Sender, notice.At, action, subjectID); err != nil {
		return nil, err
	}
	return in, call.EmitChat(in.ConversationID, notice.Seq, "message.created", ids, notice)
}

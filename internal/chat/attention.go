package chat

import (
	"context"
	"encoding/json"

	"csac/v2/internal/rpc"
)

// RegisterAttention registers the read-only inbox for unread group mentions and replies.
func RegisterAttention(r *rpc.Registry) {
	r.Register("chat.attention", rpc.Method{Access: rpc.Member, Handle: attention})
}

type attentionMessage struct {
	messageCreated
	ReplyTo *int64 `json:"reply_to,string,omitempty"`
}

type attentionInbox struct {
	UnreadMentions int64              `json:"unread_mentions"`
	UnreadReplies  int64              `json:"unread_replies"`
	Mentions       []attentionMessage `json:"mentions"`
	Replies        []attentionMessage `json:"replies"`
	Limit          int                `json:"limit"`
}

func attention(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		Limit *int `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	out := attentionInbox{Limit: 50}
	if in.Limit != nil {
		out.Limit = *in.Limit
	}
	if out.Limit < 0 || out.Limit > 100 {
		return nil, rpc.Invalid("Limit must be 0–100")
	}
	if err := call.DB.QueryRow(ctx, attentionQuery, call.Actor.ID, out.Limit).Scan(
		&out.UnreadMentions, &out.UnreadReplies, &out.Mentions, &out.Replies,
	); err != nil {
		return nil, err
	}
	return out, nil
}

// Counts and lists use one statement snapshot, with separate indexed paths for
// array containment and replies to the actor's own messages. A message can
// appear in both categories. Only the two limited lists are serialized in SQL.
const attentionQuery = `
WITH groups AS MATERIALIZED (
	SELECT cm.conversation_id, cm.read_seq
	FROM conversation_members cm
	JOIN conversations c ON c.id = cm.conversation_id
	WHERE cm.user_id = $1 AND c.kind = 'group' AND c.archived_at IS NULL
		AND (c.site_ban_until IS NULL OR c.site_ban_until <= statement_timestamp())
), mentions AS MATERIALIZED (
	SELECT m.conversation_id, m.seq, m.sender_id, m.kind, m.created_at, m.reply_seq
	FROM groups g
	JOIN messages m ON m.conversation_id = g.conversation_id AND m.seq > g.read_seq
	WHERE m.recalled_at IS NULL AND m.sender_id <> $1
		AND cardinality(m.mention_ids) > 0 AND m.mention_ids @> ARRAY[$1::bigint]
), replies AS MATERIALIZED (
	SELECT m.conversation_id, m.seq, m.sender_id, m.kind, m.created_at, m.reply_seq
	FROM groups g
	JOIN messages original ON original.conversation_id = g.conversation_id
		AND original.sender_id = $1 AND original.recalled_at IS NULL
	JOIN messages m ON m.conversation_id = original.conversation_id AND m.reply_seq = original.seq
		AND m.seq > g.read_seq
	WHERE m.recalled_at IS NULL AND m.sender_id <> $1 AND m.reply_seq IS NOT NULL
), recent AS (
	(SELECT 'mention' AS category, m.* FROM mentions m
		ORDER BY created_at DESC, conversation_id DESC, seq DESC LIMIT $2)
	UNION ALL
	(SELECT 'reply' AS category, m.* FROM replies m
		ORDER BY created_at DESC, conversation_id DESC, seq DESC LIMIT $2)
), projected AS (
	SELECT category, created_at, conversation_id, seq, jsonb_build_object(
		'conversation_id', conversation_id, 'seq', seq::text, 'sender_id', sender_id,
		'kind', kind, 'created_at', created_at, 'reply_to', reply_seq::text
	) AS item
	FROM recent
)
SELECT (SELECT count(*) FROM mentions), (SELECT count(*) FROM replies),
	coalesce(jsonb_agg(item ORDER BY created_at DESC, conversation_id DESC, seq DESC)
		FILTER (WHERE category = 'mention'), '[]'::jsonb),
	coalesce(jsonb_agg(item ORDER BY created_at DESC, conversation_id DESC, seq DESC)
		FILTER (WHERE category = 'reply'), '[]'::jsonb)
FROM projected`

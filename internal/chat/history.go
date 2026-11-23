package chat

import (
	"context"
	"encoding/json"

	"csac/v2/internal/rpc"
)

type replyPreview struct {
	Seq        int64   `json:"seq,string"`
	Sender     *int64  `json:"sender_id,omitempty"`
	SenderName *string `json:"sender_name,omitempty"`
	Kind       *string `json:"kind,omitempty"`
	Preview    *string `json:"preview,omitempty"`
	Recalled   bool    `json:"recalled"`
}

type messageDetails struct {
	message
	SenderAvatarID  *string       `json:"sender_avatar_id,omitempty"`
	Quote           *replyPreview `json:"reply,omitempty"`
	CanRecall       bool          `json:"can_recall"`
	RecallKind      *string       `json:"recall_kind,omitempty"`
	ReadByRecipient *bool         `json:"read_by_recipient,omitempty"`
}

// Authorization and all visible details use the same statement snapshot. Do
// not include quote contents in durable events: the original may be recalled
// later, while the reply itself remains readable and replayable.
const messageDetailsQuery = `SELECT m.seq,m.sender_id,u.nickname,u.avatar_id::text,m.kind,
	CASE WHEN m.recalled_at IS NULL THEN m.body ELSE '' END,
	CASE WHEN m.recalled_at IS NULL THEN m.attachment_id::text END,CASE WHEN m.recalled_at IS NULL THEN m.duration_ms END,
	CASE WHEN m.recalled_at IS NULL THEN m.reply_seq END,CASE WHEN m.recalled_at IS NULL THEN m.mention_ids ELSE '{}'::bigint[] END,
	m.created_at,m.recalled_at,
	CASE WHEN m.recalled_at IS NULL THEN m.nudge_target_id END,
	f.seq IS NOT NULL,f.added_by,setter.nickname,f.added_at,
	CASE WHEN c.kind='group' THEN coalesce(roster.level,1)::smallint END,
	CASE WHEN c.kind='group' THEN coalesce(nullif(roster.title,''),group_default_title(coalesce(roster.level,1)::smallint)) END,
	CASE WHEN m.recalled_at IS NOT NULL THEN coalesce(m.recall_kind,'unknown') END,
	m.recalled_at IS NULL AND chat_recall_kind(m.sender_id,$1,m.kind,coalesce(c.owner_id=m.sender_id,false),
		CASE WHEN c.kind='group' THEN coalesce(viewer.role,'') ELSE '' END,
		c.kind='group' AND EXISTS (SELECT 1 FROM active_site_admins WHERE user_id=$1),m.created_at,statement_timestamp()) IS NOT NULL,
	CASE WHEN c.kind='direct' THEN m.seq<=recipient.read_seq END,
	quote.recalled_at IS NOT NULL,
	CASE WHEN quote.recalled_at IS NULL THEN quote.sender_id END,
	CASE WHEN quote.recalled_at IS NULL THEN quoted_user.nickname END,
	CASE WHEN quote.recalled_at IS NULL THEN quote.kind END,
	CASE WHEN quote.recalled_at IS NULL THEN left(quote.body,160) END,
	m.system_action,m.system_subject_id,subject.nickname
	FROM messages m JOIN conversations c ON c.id=m.conversation_id
	LEFT JOIN conversation_members viewer ON viewer.conversation_id=m.conversation_id AND viewer.user_id=$1
	JOIN accounts u ON u.id=m.sender_id
	LEFT JOIN accounts subject ON subject.id=m.system_subject_id
	LEFT JOIN featured_messages f ON f.conversation_id=m.conversation_id AND f.seq=m.seq
		AND m.recalled_at IS NULL AND f.added_at<=statement_timestamp()
	LEFT JOIN accounts setter ON setter.id=f.added_by
	LEFT JOIN conversation_members roster ON c.kind='group' AND roster.conversation_id=m.conversation_id AND roster.user_id=m.sender_id
	LEFT JOIN conversation_members recipient ON c.kind='direct' AND recipient.conversation_id=m.conversation_id
		AND recipient.user_id=CASE WHEN m.sender_id=c.direct_low THEN c.direct_high ELSE c.direct_low END
	LEFT JOIN messages quote ON quote.conversation_id=m.conversation_id AND quote.seq=m.reply_seq AND m.recalled_at IS NULL
	LEFT JOIN accounts quoted_user ON quoted_user.id=quote.sender_id AND quote.recalled_at IS NULL
	WHERE m.conversation_id=$2
	AND EXISTS (SELECT 1 FROM chat_reviewers a WHERE a.conversation_id=m.conversation_id AND a.user_id=$1)`

func scanMessageDetails(row interface{ Scan(...any) error }, room int64) (messageDetails, error) {
	var m messageDetails
	var quote replyPreview
	var level *int16
	var title *string
	m.ConversationID = room
	if err := row.Scan(&m.Seq, &m.Sender, &m.Nickname, &m.SenderAvatarID, &m.Kind, &m.Text, &m.Attachment, &m.Duration,
		&m.Reply, &m.Mentions, &m.At, &m.RecalledAt, &m.TargetID,
		&m.Featured, &m.FeaturedBy, &m.FeaturedByName, &m.FeaturedAt, &level, &title,
		&m.RecallKind, &m.CanRecall, &m.ReadByRecipient,
		&quote.Recalled, &quote.Sender, &quote.SenderName, &quote.Kind, &quote.Preview,
		&m.SystemAction, &m.SystemSubject, &m.SystemSubjectName); err != nil {
		return m, err
	}
	if level != nil && title != nil {
		m.groupActivity = &groupActivity{MemberLevel: *level, MemberTitle: *title}
	}
	if m.FeaturedAt != nil {
		utc := m.FeaturedAt.UTC()
		m.FeaturedAt = &utc
	}
	if m.Reply != nil {
		quote.Seq = *m.Reply
		m.Quote = &quote
	}
	return m, nil
}

func getMessage(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[messageRef](raw)
	if err != nil {
		return nil, err
	}
	if in.ConversationID <= 0 || in.Seq <= 0 {
		return nil, rpc.Invalid("Choose a conversation and message sequence")
	}
	if _, err := reviewAccessTo(ctx, call.DB, in.ConversationID, call.Actor.ID, false); err != nil {
		return nil, err
	}
	return scanMessageDetails(call.DB.QueryRow(ctx, messageDetailsQuery+` AND m.seq=$3`, call.Actor.ID, in.ConversationID, in.Seq), in.ConversationID)
}

type pageInput struct {
	conversationRef
	Before int64 `json:"before,string"`
	After  int64 `json:"after,string"`
	Limit  int   `json:"limit"`
}

func history(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	return messagePage(ctx, call, raw, false)
}

func featured(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	return messagePage(ctx, call, raw, true)
}

func messagePage(ctx context.Context, call rpc.Call, raw json.RawMessage, featuredOnly bool) (any, error) {
	in, err := rpc.Decode[pageInput](raw)
	if err != nil {
		return nil, err
	}
	if in.ConversationID <= 0 || in.Before < 0 || in.After < 0 || (in.Before > 0 && in.After > 0) {
		return nil, rpc.Invalid("Invalid timeline cursor")
	}
	if in.Limit == 0 {
		in.Limit = 50
	}
	if in.Limit < 1 || in.Limit > 100 {
		return nil, rpc.Invalid("Limit must be 1–100")
	}
	if _, err := reviewAccessTo(ctx, call.DB, in.ConversationID, call.Actor.ID, false); err != nil {
		return nil, err
	}
	query := messageDetailsQuery + ` AND ($3::bigint=0 OR m.seq<$3) AND ($4::bigint=0 OR m.seq>$4)
		AND (NOT $5::boolean OR f.seq IS NOT NULL)`
	if in.After > 0 {
		query += ` ORDER BY m.seq ASC`
	} else {
		query += ` ORDER BY m.seq DESC`
	}
	rows, err := call.DB.Query(ctx, query+` LIMIT $6`, call.Actor.ID, in.ConversationID, in.Before, in.After, featuredOnly, in.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]messageDetails, 0, in.Limit)
	bytesUsed := 0
	more := false
	for rows.Next() {
		m, err := scanMessageDetails(rows, in.ConversationID)
		if err != nil {
			return nil, err
		}
		// Allow for JSON escaping in imported legacy messages and response framing.
		bytesUsed += 6*(len(m.Text)+len(m.Nickname)) + 2048
		if m.Quote != nil {
			if m.Quote.Preview != nil {
				bytesUsed += 6 * len(*m.Quote.Preview)
			}
			if m.Quote.SenderName != nil {
				bytesUsed += 6 * len(*m.Quote.SenderName)
			}
		}
		if len(out) > 0 && (len(out) == in.Limit || bytesUsed > 700<<10) {
			more = true
			break
		}
		out = append(out, m)
	}
	return struct {
		Messages []messageDetails `json:"messages"`
		More     bool             `json:"has_more"`
	}{out, more}, rows.Err()
}

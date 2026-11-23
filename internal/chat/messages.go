package chat

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"csac/v2/internal/rpc"
	"github.com/jackc/pgx/v5"
)

var objectID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

type sendInput struct {
	conversationRef
	Kind       string  `json:"kind"`
	Text       string  `json:"text"`
	Attachment string  `json:"attachment_id"`
	Duration   *int32  `json:"duration_ms"`
	Reply      *int64  `json:"reply_to,string"`
	Mentions   []int64 `json:"mention_ids"`
	Sticker    string  `json:"sticker"`
	Target     int64   `json:"target_id"`
}

func parseSend(raw json.RawMessage) (sendInput, error) {
	in, err := rpc.Decode[sendInput](raw)
	if err != nil {
		return in, err
	}
	if in.ConversationID <= 0 || len(in.Text) > 8000 || len(in.Mentions) > 100 || (in.Reply != nil && *in.Reply <= 0) {
		return in, rpc.Invalid("Invalid message fields")
	}
	if in.Mentions == nil {
		in.Mentions = []int64{}
	}
	slices.Sort(in.Mentions)
	in.Mentions = slices.Compact(in.Mentions)
	if len(in.Mentions) > 0 && in.Mentions[0] <= 0 {
		return in, rpc.Invalid("Invalid mention")
	}
	in.Attachment = strings.ToLower(in.Attachment)
	switch in.Kind {
	case "text":
		if strings.TrimSpace(in.Text) == "" {
			return in, rpc.Invalid("Message is empty")
		}
	case "image", "voice", "file":
		if !objectID.MatchString(in.Attachment) {
			return in, rpc.Invalid("Attachment ID required")
		}
	case "sticker":
		if in.Sticker == "" || len(in.Sticker) > 40 || strings.ContainsRune(in.Sticker, 0) {
			return in, rpc.Invalid("Sticker code required")
		}
		in.Text = in.Sticker
	case "nudge":
		if in.Target <= 0 {
			return in, rpc.Invalid("Nudge target required")
		}
		in.Text = ""
	default:
		return in, rpc.Invalid("Unsupported message kind")
	}
	if in.Kind == "voice" {
		if in.Duration == nil || *in.Duration <= 0 || *in.Duration > 3600000 {
			return in, rpc.Invalid("Voice duration out of range")
		}
	} else if in.Duration != nil {
		return in, rpc.Invalid("Duration is reserved for voice messages")
	}
	if in.Attachment != "" && in.Kind != "image" && in.Kind != "voice" && in.Kind != "file" {
		return in, rpc.Invalid("Attachment is unavailable for this message kind")
	}
	return in, nil
}

type messageRef struct {
	ConversationID int64 `json:"conversation_id"`
	Seq            int64 `json:"seq,string"`
}
type messageCreated struct {
	messageRef
	*groupActivity
	Sender int64     `json:"sender_id"`
	Kind   string    `json:"kind"`
	At     time.Time `json:"created_at"`
}

func send(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := parseSend(raw)
	if err != nil {
		return nil, err
	}
	uid := call.Actor.ID
	a, err := accessTo(ctx, call.DB, in.ConversationID, uid, true)
	if err != nil {
		return nil, err
	}
	if a.muted {
		return nil, rpc.Denied("You are muted in this group")
	}
	members, err := memberIDs(ctx, call.DB, in.ConversationID)
	if err != nil {
		return nil, err
	}
	for _, id := range in.Mentions {
		if !slices.Contains(members, id) {
			return nil, rpc.Invalid("Mentioned user is outside this conversation")
		}
	}
	var nudgeTarget *int64
	switch in.Kind {
	case "nudge":
		if in.Target == uid || !slices.Contains(members, in.Target) {
			return nil, rpc.Invalid("Nudge target is outside this conversation or is the sender")
		}
		in.Text, err = renderNudge(ctx, call.DB, uid, in.Target)
		if err != nil {
			return nil, err
		}
		nudgeTarget = &in.Target
	case "sticker":
		if err := requireSticker(ctx, call.DB, in.Sticker); err != nil {
			return nil, err
		}
	}
	if in.Attachment != "" {
		var mime string
		err := call.DB.QueryRow(ctx, `SELECT mime FROM media_objects WHERE id=$1::uuid AND owner_id=$2 AND purpose='message' AND state='ready' AND (expires_at IS NULL OR expires_at>now()) FOR SHARE`, in.Attachment, uid).Scan(&mime)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, rpc.NotFound("Attachment unavailable")
		}
		if err != nil {
			return nil, err
		}
		if (in.Kind == "image" && !strings.HasPrefix(mime, "image/")) || (in.Kind == "voice" && !strings.HasPrefix(mime, "audio/")) {
			return nil, rpc.Invalid("Attachment MIME does not match message kind")
		}
	}
	var replyAuthor int64
	if in.Reply != nil {
		err := call.DB.QueryRow(ctx, `SELECT sender_id FROM messages WHERE conversation_id=$1 AND seq=$2 AND recalled_at IS NULL`, in.ConversationID, *in.Reply).Scan(&replyAuthor)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, rpc.Invalid("Reply target unavailable")
		}
		if err != nil {
			return nil, err
		}
	}
	var m messageCreated
	m.ConversationID, m.Sender, m.Kind = in.ConversationID, uid, in.Kind
	if err = call.DB.QueryRow(ctx, `UPDATE conversation_heads SET head=head+1,last_sent_at=clock_timestamp() WHERE conversation_id=$1 RETURNING head,last_sent_at`, in.ConversationID).Scan(&m.Seq, &m.At); err != nil {
		return nil, err
	}
	if _, err = call.DB.Exec(ctx, `INSERT INTO messages(conversation_id,seq,sender_id,kind,body,attachment_id,duration_ms,reply_seq,mention_ids,created_at,nudge_target_id)
		VALUES ($1,$2,$3,$4,$5,nullif($6,'')::uuid,$7,$8,$9,$10,$11)`, in.ConversationID, m.Seq, uid, in.Kind, in.Text, in.Attachment, in.Duration, in.Reply, in.Mentions, m.At, nudgeTarget); err != nil {
		return nil, err
	}
	var experience *experienceAward
	if a.kind == "group" {
		m.groupActivity, experience, err = recordGroupActivity(ctx, call.DB, in.ConversationID, uid, m.At, in.Kind)
		if err != nil {
			return nil, err
		}
	}

	for _, notice := range []struct {
		kind   string
		target []int64
	}{{"message.mention", slices.DeleteFunc(slices.Clone(in.Mentions), func(id int64) bool { return id == uid })}, {"message.reply", replyNotice(replyAuthor, uid, members)}} {
		if len(notice.target) == 0 {
			continue
		}
		payload, _ := json.Marshal(m.messageRef)
		if _, err := call.DB.Exec(ctx, `INSERT INTO notifications(user_id,kind,data) SELECT unnest($1::bigint[]),$2,$3::jsonb`, notice.target, notice.kind, payload); err != nil {
			return nil, err
		}
		if err := call.Emit("notice.created", notice.target, struct {
			Kind    string     `json:"kind"`
			Message messageRef `json:"message"`
		}{notice.kind, m.messageRef}); err != nil {
			return nil, err
		}
	}
	push := message{messageCreated: m, Text: in.Text, Duration: in.Duration, Reply: in.Reply, Mentions: in.Mentions, TargetID: nudgeTarget}
	if in.Attachment != "" {
		push.Attachment = &in.Attachment
	}
	if err := call.EmitChat(in.ConversationID, m.Seq, "message.created", members, push); err != nil {
		return nil, err
	}
	if in.Kind == "nudge" {
		return struct {
			message
			Experience *experienceAward `json:"group_experience,omitempty"`
		}{push, experience}, nil
	}
	return struct {
		messageCreated
		Experience *experienceAward `json:"group_experience,omitempty"`
	}{m, experience}, nil
}

func replyNotice(author, sender int64, members []int64) []int64 {
	if author <= 0 || author == sender || !slices.Contains(members, author) {
		return nil
	}
	return []int64{author}
}

type message struct {
	messageCreated
	Nickname       string     `json:"sender_name,omitempty"`
	Text           string     `json:"text,omitempty"`
	Attachment     *string    `json:"attachment_id,omitempty"`
	Duration       *int32     `json:"duration_ms,omitempty"`
	TargetID       *int64     `json:"target_id,omitempty"`
	Reply          *int64     `json:"reply_to,string,omitempty"`
	Mentions       []int64    `json:"mention_ids,omitempty"`
	RecalledAt     *time.Time `json:"recalled_at,omitempty"`
	Featured       bool       `json:"featured,omitempty"`
	FeaturedBy     *int64     `json:"featured_by,omitempty"`
	FeaturedByName *string    `json:"featured_by_name,omitempty"`
	FeaturedAt     *time.Time `json:"featured_at,omitempty"`
}

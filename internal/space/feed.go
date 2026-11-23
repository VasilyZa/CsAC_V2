package space

import (
	"context"
	"encoding/json"
	"time"

	"csac/v2/internal/rpc"
	"github.com/jackc/pgx/v5"
)

const previewReplies = 3

type item struct {
	ID        int64     `json:"id"`
	AuthorID  int64     `json:"author_id"`
	ParentID  *int64    `json:"parent_id,omitempty"`
	ReplyToID *int64    `json:"reply_to_id,omitempty"`
	Nickname  string    `json:"nickname"`
	AvatarID  *string   `json:"avatar_id,omitempty"`
	Content   string    `json:"content"`
	MediaIDs  []string  `json:"media_ids"`
	CreatedAt time.Time `json:"created_at"`
	Likes     int64     `json:"likes_count"`
	Liked     bool      `json:"liked"`
}

type post struct {
	item
	Replies        []item `json:"replies"`
	NextReplyAfter int64  `json:"next_reply_after_id,omitempty"`
}

const itemColumns = `p.id,p.author_id,p.parent_id,
	(SELECT target.id FROM space_posts target WHERE target.id=p.reply_to_id
		AND space_can_view_item($1,root.author_id,target.author_id)),
	a.nickname,a.avatar_id::text,p.content,
	ARRAY(SELECT pm.media_id::text FROM space_post_media pm JOIN media_objects m ON m.id=pm.media_id
		WHERE pm.post_id=p.id AND m.state='ready' AND m.purpose='space' AND m.purged_at IS NULL
		AND (m.expires_at IS NULL OR m.expires_at>statement_timestamp()) ORDER BY pm.position),
	p.created_at,
	(SELECT count(*) FROM space_likes l WHERE l.post_id=p.id AND space_can_view_item(l.user_id,coalesce(root.author_id,p.author_id),p.author_id)),
	EXISTS (SELECT 1 FROM space_likes l WHERE l.post_id=p.id AND l.user_id=$1)`

const visibleFeedRoots = `p.parent_id IS NULL AND p.author_id IN (SELECT id FROM audience WHERE space_can_view($1,id))
	AND ($2::bigint=0 OR p.author_id=$2)`

func feed(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		AuthorID     int64 `json:"author_id"`
		Before       int64 `json:"before_id"`
		Limit        int   `json:"limit"`
		IncludeTotal bool  `json:"include_total"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.Limit == 0 {
		in.Limit = 20
	}
	if in.AuthorID < 0 || in.Before < 0 || in.Limit < 1 || in.Limit > 50 {
		return nil, rpc.Invalid("Invalid space feed page")
	}
	if in.AuthorID > 0 {
		var allowed bool
		if err := call.DB.QueryRow(ctx, `SELECT space_can_view($1,$2)`, call.Actor.ID, in.AuthorID).Scan(&allowed); err != nil {
			return nil, err
		}
		if !allowed {
			return nil, rpc.NotFound("Space feed unavailable")
		}
	}
	rows, err := call.DB.Query(ctx, `WITH audience AS (`+audienceCandidates+`)
		SELECT `+itemColumns+`
		FROM space_posts p JOIN accounts a ON a.id=p.author_id LEFT JOIN space_posts root ON root.id=p.parent_id
		WHERE `+visibleFeedRoots+` AND ($3=0 OR p.id<$3) ORDER BY p.id DESC LIMIT $4`, call.Actor.ID, in.AuthorID, in.Before, in.Limit+1)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[item])
	if err != nil {
		return nil, err
	}
	var next int64
	if len(items) > in.Limit {
		items, next = items[:in.Limit], items[in.Limit-1].ID
	}
	ids := make([]int64, len(items))
	posts := make([]post, len(items))
	for i, entry := range items {
		ids[i] = entry.ID
		posts[i] = post{item: entry, Replies: []item{}}
	}
	if err := loadReplyPreviews(ctx, call, ids, posts); err != nil {
		return nil, err
	}
	var total *int64
	if in.IncludeTotal {
		total = new(int64)
		if err := call.DB.QueryRow(ctx, `WITH audience AS (`+audienceCandidates+`)
			SELECT count(*) FROM space_posts p WHERE `+visibleFeedRoots, call.Actor.ID, in.AuthorID).Scan(total); err != nil {
			return nil, err
		}
	}
	return struct {
		Posts  []post `json:"posts"`
		Before int64  `json:"next_before_id,omitempty"`
		Total  *int64 `json:"total,omitempty"`
	}{posts, next, total}, nil
}

func loadReplyPreviews(ctx context.Context, call rpc.Call, ids []int64, posts []post) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := call.DB.Query(ctx, `SELECT `+itemColumns+`
		FROM unnest($2::bigint[]) AS ids(id) JOIN space_posts root ON root.id=ids.id AND root.parent_id IS NULL
		CROSS JOIN LATERAL (SELECT child.* FROM space_posts child WHERE child.parent_id=root.id
		  AND space_can_view_item($1,root.author_id,child.author_id) ORDER BY child.id LIMIT $3) p
		JOIN accounts a ON a.id=p.author_id ORDER BY p.id`, call.Actor.ID, ids, previewReplies+1)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[item])
	if err != nil {
		return err
	}
	indices := make(map[int64]int, len(ids))
	for i, id := range ids {
		indices[id] = i
	}
	for _, reply := range items {
		p := &posts[indices[*reply.ParentID]]
		if len(p.Replies) == previewReplies {
			p.NextReplyAfter = p.Replies[previewReplies-1].ID
		} else {
			p.Replies = append(p.Replies, reply)
		}
	}
	return nil
}

func replies(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		postRef
		After int64 `json:"after_id"`
		Limit int   `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.Limit == 0 {
		in.Limit = 20
	}
	if in.PostID <= 0 || in.After < 0 || in.Limit < 1 || in.Limit > 50 {
		return nil, rpc.Invalid("Invalid space reply page")
	}
	var visible bool
	err = call.DB.QueryRow(ctx, `SELECT true FROM space_posts WHERE id=$2 AND parent_id IS NULL AND space_can_view($1,author_id)`,
		call.Actor.ID, in.PostID).Scan(&visible)
	if err != nil {
		return nil, postUnavailable(err)
	}
	rows, err := call.DB.Query(ctx, `SELECT `+itemColumns+`
		FROM space_posts p JOIN accounts a ON a.id=p.author_id JOIN space_posts root ON root.id=p.parent_id
		WHERE root.id=$2 AND root.parent_id IS NULL AND p.id>$3 AND space_can_view_item($1,root.author_id,p.author_id)
		ORDER BY p.id LIMIT $4`, call.Actor.ID, in.PostID, in.After, in.Limit+1)
	if err != nil {
		return nil, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByPos[item])
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []item{}
	}
	var next int64
	if len(items) > in.Limit {
		items, next = items[:in.Limit], items[in.Limit-1].ID
	}
	return struct {
		Replies []item `json:"replies"`
		After   int64  `json:"next_after_id,omitempty"`
	}{items, next}, nil
}

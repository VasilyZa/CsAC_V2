// Package space owns the friend-visible social feed and its interactions.
package space

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"csac/v2/internal/media"
	"csac/v2/internal/rpc"
	"github.com/jackc/pgx/v5"
)

func Register(reg *rpc.Registry) {
	reg.Register("space.feed", rpc.Method{Access: rpc.Member, Handle: feed})
	reg.Register("space.replies", rpc.Method{Access: rpc.Member, Handle: replies})
	reg.Register("space.create", rpc.Method{Access: rpc.Member, Write: true, Dedup: true, Handle: create})
	reg.Register("space.reply", rpc.Method{Access: rpc.Member, Write: true, Dedup: true, Handle: reply})
	reg.Register("space.delete", rpc.Method{Access: rpc.Member, Write: true, Dedup: true, Handle: remove})
	reg.Register("space.like.toggle", rpc.Method{Access: rpc.Member, Write: true, Dedup: true, Handle: toggleLike})
}

type contentInput struct {
	Content  string   `json:"content"`
	MediaIDs []string `json:"media_ids"`
}

type postRef struct {
	PostID int64 `json:"post_id"`
}

type idResult struct {
	ID int64 `json:"id"`
}

type audience struct {
	rootAuthor int64
	itemAuthor int64
}

func create(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[contentInput](raw)
	if err != nil {
		return nil, err
	}
	return addItem(ctx, call, in, nil, nil, audience{call.Actor.ID, call.Actor.ID})
}

func reply(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		postRef
		contentInput
		ReplyToID *int64 `json:"reply_to_id"`
	}](raw)
	if err != nil {
		return nil, err
	}
	viewers, err := lockVisible(ctx, call, in.PostID, true)
	if err != nil {
		return nil, err
	}
	if in.ReplyToID != nil {
		if *in.ReplyToID <= 0 {
			return nil, rpc.Invalid("Expected a reply ID")
		}
		var visible bool
		err := call.DB.QueryRow(ctx, `SELECT true FROM space_posts p WHERE p.id=$1 AND p.parent_id=$2
			AND space_can_view_item($3,$4,p.author_id) FOR KEY SHARE`, *in.ReplyToID, in.PostID, call.Actor.ID, viewers.rootAuthor).Scan(&visible)
		if err != nil {
			return nil, postUnavailable(err)
		}
	}
	return addItem(ctx, call, in.contentInput, &in.PostID, in.ReplyToID, audience{viewers.rootAuthor, call.Actor.ID})
}

func addItem(ctx context.Context, call rpc.Call, in contentInput, parent, replyTo *int64, viewers audience) (any, error) {
	// Keep Markdown indentation and line breaks when storing non-empty text.
	content := in.Content
	blank := strings.TrimSpace(content) == ""
	if len(content) > 4000 || strings.ContainsRune(content, 0) || len(in.MediaIDs) > 9 || (blank && len(in.MediaIDs) == 0) {
		return nil, rpc.Invalid("Provide up to 4000 bytes of text or up to nine images")
	}
	if blank {
		content = "分享图片"
	}
	ids := slices.Clone(in.MediaIDs)
	slices.Sort(ids)
	for i, id := range ids {
		if i != 0 && id == ids[i-1] {
			return nil, rpc.Invalid("Media IDs must be distinct")
		}
		if err := media.RequireOwnedReady(ctx, call.DB, call.Actor.ID, id, "space"); err != nil {
			return nil, err
		}
	}
	var out idResult
	if err := call.DB.QueryRow(ctx, `INSERT INTO space_posts(author_id,parent_id,reply_to_id,content) VALUES ($1,$2,$3,$4) RETURNING id`,
		call.Actor.ID, parent, replyTo, content).Scan(&out.ID); err != nil {
		return nil, err
	}
	if len(in.MediaIDs) > 0 {
		if _, err := call.DB.Exec(ctx, `INSERT INTO space_post_media(post_id,position,media_id)
			SELECT $1,position-1,id::uuid FROM unnest($2::text[]) WITH ORDINALITY AS images(id,position)`, out.ID, in.MediaIDs); err != nil {
			return nil, err
		}
	}
	return out, emitChange(ctx, call, viewers)
}

func remove(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[postRef](raw)
	if err != nil {
		return nil, err
	}
	if in.PostID <= 0 {
		return nil, rpc.Invalid("Expected a space post ID")
	}
	var rootID, rootAuthor int64
	// Deletion can clear reply targets in other comments. Serialize deletions
	// by locking the root before touching any comments in the thread.
	err = call.DB.QueryRow(ctx, `SELECT root.id,root.author_id
		FROM space_posts p JOIN space_posts root ON root.id=coalesce(p.parent_id,p.id)
		WHERE p.id=$1 AND p.author_id=$2 AND root.parent_id IS NULL FOR UPDATE OF root`, in.PostID, call.Actor.ID).Scan(&rootID, &rootAuthor)
	if err != nil {
		return nil, postUnavailable(err)
	}
	// An author can withdraw their own reply after losing access to its root.
	deleted, err := call.DB.Exec(ctx, `DELETE FROM space_posts WHERE id=$1 AND author_id=$2 AND coalesce(parent_id,id)=$3`,
		in.PostID, call.Actor.ID, rootID)
	if err != nil {
		return nil, err
	}
	if deleted.RowsAffected() == 0 {
		return nil, rpc.NotFound("Space post unavailable")
	}
	return idResult{in.PostID}, emitChange(ctx, call, audience{rootAuthor, call.Actor.ID})
}

func toggleLike(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[postRef](raw)
	if err != nil {
		return nil, err
	}
	viewers, err := lockVisible(ctx, call, in.PostID, false)
	if err != nil {
		return nil, err
	}
	del, err := call.DB.Exec(ctx, `DELETE FROM space_likes WHERE post_id=$1 AND user_id=$2`, in.PostID, call.Actor.ID)
	if err != nil {
		return nil, err
	}
	out := struct {
		Liked bool  `json:"liked"`
		Count int64 `json:"likes_count"`
	}{Liked: del.RowsAffected() == 0}
	if out.Liked {
		if _, err = call.DB.Exec(ctx, `INSERT INTO space_likes(post_id,user_id) VALUES ($1,$2)`, in.PostID, call.Actor.ID); err != nil {
			return nil, err
		}
	}
	if err := call.DB.QueryRow(ctx, `SELECT count(*) FROM space_likes WHERE post_id=$1 AND space_can_view_item(user_id,$2,$3)`,
		in.PostID, viewers.rootAuthor, viewers.itemAuthor).Scan(&out.Count); err != nil {
		return nil, err
	}
	return out, emitChange(ctx, call, viewers)
}

func lockVisible(ctx context.Context, call rpc.Call, id int64, rootOnly bool) (audience, error) {
	if id <= 0 {
		return audience{}, rpc.Invalid("Expected a space post ID")
	}
	var viewers audience
	err := call.DB.QueryRow(ctx, `SELECT coalesce(root.author_id,p.author_id),p.author_id
		FROM space_posts p LEFT JOIN space_posts root ON root.id=p.parent_id
		WHERE p.id=$1 AND (NOT $3 OR p.parent_id IS NULL) AND root.parent_id IS NULL
		AND space_can_view_item($2,coalesce(root.author_id,p.author_id),p.author_id) FOR UPDATE OF p`,
		id, call.Actor.ID, rootOnly).Scan(&viewers.rootAuthor, &viewers.itemAuthor)
	return viewers, postUnavailable(err)
}

func postUnavailable(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return rpc.NotFound("Space post unavailable")
	}
	return err
}

const audienceCandidates = `SELECT $1::bigint AS id
	UNION ALL SELECT user_low FROM friendships WHERE user_high=$1
	UNION ALL SELECT user_high FROM friendships WHERE user_low=$1`

func emitChange(ctx context.Context, call rpc.Call, viewers audience) error {
	rows, err := call.DB.Query(ctx, `SELECT a.id FROM (`+audienceCandidates+`) v JOIN accounts a ON a.id=v.id
		WHERE a.status='active' AND space_can_view_item(a.id,$1,$2)`, viewers.rootAuthor, viewers.itemAuthor)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	return call.Emit("space.changed", ids, struct{}{})
}

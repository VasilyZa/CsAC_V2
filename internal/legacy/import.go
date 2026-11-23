package legacy

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"csac/v2/internal/schema"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Apply checks the exact review before accessing the destination. schema.Apply
// commits schema versioning separately. All imported rows and sequence changes
// are committed atomically by the transaction below.
func (p *Plan) Apply(ctx context.Context, pool *pgxpool.Pool, review Report, targetMedia string) (err error) {
	if !p.matches(review) {
		return errors.New("source data, media or review changed; generate and review a fresh report")
	}
	if p.report.Blocked() {
		return errors.New("review contains blocking findings")
	}
	if len(p.media) != 0 && targetMedia == "" {
		return errors.New("destination media root is required by the review")
	}
	if len(p.media) != 0 {
		if err := separateMediaRoots(p.sourceMedia, targetMedia); err != nil {
			return err
		}
	}
	if err := schema.Apply(ctx, pool); err != nil {
		return destinationError("apply schema", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return destinationError("begin import", err)
	}
	defer func() {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		tx.Rollback(rollbackCtx)
	}()
	if err := emptyDestination(ctx, tx); err != nil {
		return err
	}
	staged, err := p.stage(ctx, targetMedia)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if closeErr := staged.close(); closeErr != nil {
			if committed {
				err = fmt.Errorf("migration committed: %w", closeErr)
			} else {
				err = errors.Join(err, closeErr)
			}
		}
	}()
	if err := p.write(ctx, tx); err != nil {
		return err
	}
	if err := p.restartIDs(ctx, tx); err != nil {
		return err
	}
	if err := p.verifyDestination(ctx, tx); err != nil {
		return err
	}
	reportBytes, err := p.report.json()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO legacy_migration(fingerprint,review) VALUES ($1,$2)`, p.report.Fingerprint, reportBytes); err != nil {
		return destinationError("record reviewed import", err)
	}
	keys, err := p.storedKeys(ctx, tx)
	if err != nil {
		return err
	}
	if err := staged.publish(ctx, p.media, keys); err != nil {
		return errors.Join(err, staged.removePublished())
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, staged.removePublished())
	}
	if err := tx.Commit(ctx); err != nil {
		if errors.Is(err, pgx.ErrTxCommitRollback) {
			return errors.Join(errors.New("destination transaction rolled back"), staged.removePublished())
		}
		// A lost commit acknowledgment can leave committed DB rows. Leave
		// published files for inspection or a same-review retry.
		return fmt.Errorf("destination commit outcome requires checking legacy_migration: %w", destinationError("commit import", err))
	}
	committed = true
	return nil
}

var importTables = []string{"accounts", "user_activity", "conversations", "conversation_members", "conversation_heads", "friendships", "friend_preferences",
	"friend_requests", "group_join_requests", "group_transfers", "notifications", "friend_deletions", "group_member_experience", "messages", "featured_messages", "media_objects", "stickers",
	"group_file_folders", "group_files", "space_posts", "space_post_media", "space_likes", "reports", "legacy_message_map"}

var operationalTables = []string{"legacy_migration", "events", "command_keys", "account_sessions", "account_email_challenges",
	"account_registration_codes", "account_registration_redemptions"}

func emptyDestination(ctx context.Context, tx pgx.Tx) error {
	tables := append(append([]string(nil), importTables...), operationalTables...)
	quoted := make([]string, 0, len(tables))
	for _, table := range tables {
		quoted = append(quoted, pgx.Identifier{table}.Sanitize())
	}
	if _, err := tx.Exec(ctx, `LOCK TABLE event_clock,`+strings.Join(quoted, ",")+` IN SHARE ROW EXCLUSIVE MODE`); err != nil {
		return destinationError("lock destination", err)
	}
	var pristine bool
	if err := tx.QueryRow(ctx, `SELECT head=0 AND retained_through=0 FROM event_clock WHERE singleton`).Scan(&pristine); err != nil {
		return destinationError("read destination event clock", err)
	}
	if !pristine {
		return errors.New("destination event clock is not pristine")
	}
	for i, table := range quoted {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+table+`)`).Scan(&exists); err != nil {
			return destinationError("check empty "+tables[i], err)
		}
		if exists {
			return fmt.Errorf("destination is not empty (%s)", tables[i])
		}
	}
	return nil
}

func destinationError(action string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("PostgreSQL %s: %w", action, err)
	}
	var server *pgconn.PgError
	if errors.As(err, &server) {
		return fmt.Errorf("PostgreSQL %s failed (SQLSTATE %s)", action, server.Code)
	}
	return fmt.Errorf("PostgreSQL %s failed (%T)", action, err)
}

func copyRows[T any](ctx context.Context, tx pgx.Tx, table, columns string, rows []T, fields func(T) []any) error {
	if len(rows) == 0 {
		return nil
	}
	count, err := tx.CopyFrom(ctx, pgx.Identifier{table}, strings.Fields(columns), pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		return fields(rows[i]), nil
	}))
	if err != nil {
		return destinationError("copy "+table, err)
	}
	if count != int64(len(rows)) {
		return formatRowsMismatch(table, count, int64(len(rows)))
	}
	return nil
}

func (p *Plan) write(ctx context.Context, tx pgx.Tx) error {
	if err := copyRows(ctx, tx, "accounts", `id username nickname password_hash email email_verified registration_grant site_admin status
		allow_auto_join pat_action is_first_login ban_until ban_reason deleted_at purged_at created_at`, p.accounts, func(a accountRow) []any {
		return []any{a.ID, a.Username, a.Nickname, a.Password, nonempty(a.Email), a.Email != "", a.Grant, false, a.Status, a.AutoJoin,
			a.Pat, a.FirstLogin, a.BannedUntil, a.BanReason, a.DeletedAt, a.PurgedAt, a.Created}
	}); err != nil {
		return err
	}
	activity := make([]accountRow, 0, p.report.Planned["user_activity"])
	for _, account := range p.accounts {
		if account.LastActive != nil {
			activity = append(activity, account)
		}
	}
	if err := copyRows(ctx, tx, "user_activity", `user_id last_active`, activity, func(a accountRow) []any {
		return []any{a.ID, a.LastActive}
	}); err != nil {
		return err
	}
	if err := copyRows(ctx, tx, "media_objects", `id owner_id state purpose mime filename size created_at ready_at expires_at`, p.media, func(m mediaItem) []any {
		return []any{m.ID, m.Ref.Owner, "ready", m.Purpose, m.MIME, m.Name, m.Size, m.Ref.Created, m.Ref.Created, m.Ref.Expiry}
	}); err != nil {
		return err
	}
	var avatarOwners []int64
	var avatars []pgtype.UUID
	for _, a := range p.accounts {
		if a.Avatar.Valid {
			avatarOwners, avatars = append(avatarOwners, a.ID), append(avatars, a.Avatar)
		}
	}
	if len(avatars) != 0 {
		if _, err := tx.Exec(ctx, `UPDATE accounts AS a SET avatar_id=v.avatar_id
			FROM unnest($1::bigint[], $2::uuid[]) AS v(id,avatar_id) WHERE a.id=v.id`, avatarOwners, avatars); err != nil {
			return destinationError("assign account avatars", err)
		}
	}
	if err := copyRows(ctx, tx, "conversations", `id kind title description announcement avatar_id owner_id direct_low direct_high
		visibility join_policy invite_code fixed_code allow_invite code_single_use join_question discoverable archived_at
		site_ban_until site_ban_reason owner_transfer_cooldown_until created_at`, p.conversations, func(c conversationRow) []any {
		return []any{c.ID, c.Kind, c.Title, c.Intro, c.Notice, c.Avatar, ptrIfPositive(c.Owner), ptrIfPositive(c.Low), ptrIfPositive(c.High),
			c.Visibility, c.Policy, nonempty(c.Code), nonempty(c.FixedCode), c.AllowInvite, c.SingleUse, c.Question, c.Discoverable,
			c.Archived, c.SiteBanUntil, c.SiteBanReason, c.TransferCooldownUntil, c.Created}
	}); err != nil {
		return err
	}
	if err := copyRows(ctx, tx, "conversation_members", `conversation_id user_id role title level joined_at muted_until hidden read_seq level_custom`, p.members, func(m memberRow) []any {
		return []any{m.Conversation, m.User, m.Role, m.Title, m.Level, m.Joined, m.Mute, m.Hidden, m.Read, m.LevelCustom}
	}); err != nil {
		return err
	}
	if err := copyRows(ctx, tx, "conversation_heads", `conversation_id head last_sent_at`, p.conversations, func(c conversationRow) []any {
		return []any{c.ID, c.Head, c.LastSent}
	}); err != nil {
		return err
	}
	if err := p.writeSocial(ctx, tx); err != nil {
		return err
	}
	if err := p.writeHistory(ctx, tx); err != nil {
		return err
	}
	if err := p.writeContent(ctx, tx); err != nil {
		return err
	}
	return copyRows(ctx, tx, "reports", `id reporter_id target_kind target_id title reason anonymous state action_note created_at updated_at`,
		p.reports, func(r moderationRow) []any {
			return []any{r.ID, ptrIfPositive(r.Reporter), r.Kind, r.Target, r.Title, r.Reason, r.Anonymous, r.State, r.Note, r.Created, r.Updated}
		})
}

func (p *Plan) writeSocial(ctx context.Context, tx pgx.Tx) error {
	if err := copyRows(ctx, tx, "friendships", `user_low user_high created_at`, p.friends, func(r friendshipRow) []any {
		return []any{r.Pair.Low, r.Pair.High, r.Created}
	}); err != nil {
		return err
	}
	if err := copyRows(ctx, tx, "friend_preferences", `owner_id other_id blocked remark`, p.preferences, func(r preferenceRow) []any {
		return []any{r.Owner, r.Other, r.Blocked, r.Remark}
	}); err != nil {
		return err
	}
	if err := copyRows(ctx, tx, "friend_requests", `id sender_id recipient_id kind message status created_at resolved_at`, p.friendRequests, func(r requestRow) []any {
		return []any{r.ID, r.From, r.To, r.Kind, r.Text, r.Status, r.Created, r.Resolved}
	}); err != nil {
		return err
	}
	if err := copyRows(ctx, tx, "group_join_requests", `id conversation_id user_id kind answer status created_at resolved_at`, p.groupRequests, func(r requestRow) []any {
		return []any{r.ID, r.Room, r.To, r.Kind, r.Text, r.Status, r.Created, r.Resolved}
	}); err != nil {
		return err
	}
	if err := copyRows(ctx, tx, "group_transfers", `id conversation_id from_user_id to_user_id status created_at resolved_at`, p.transfers, func(r requestRow) []any {
		return []any{r.ID, r.Room, r.From, r.To, r.Status, r.Created, r.Resolved}
	}); err != nil {
		return err
	}
	if err := copyRows(ctx, tx, "notifications", `id user_id kind data created_at read_at`, p.notices, func(n noticeRow) []any {
		return []any{n.ID, n.User, n.Kind, n.Data, n.Created, n.Read}
	}); err != nil {
		return err
	}
	return copyRows(ctx, tx, "friend_deletions", `notification_id user_low user_high deleted_by friendship_created_at`, p.deletions, func(d friendDeletionRow) []any {
		return []any{d.Notice, d.Pair.Low, d.Pair.High, d.Actor, d.FriendshipCreated}
	})
}

func (p *Plan) writeHistory(ctx context.Context, tx pgx.Tx) error {
	if err := copyRows(ctx, tx, "stickers", `code label media_id`, p.stickers, func(s stickerRow) []any {
		return []any{s.Code, s.Label, s.Media}
	}); err != nil {
		return err
	}
	if err := copyRows(ctx, tx, "messages", `conversation_id seq sender_id kind body attachment_id duration_ms reply_seq
		mention_ids created_at recalled_at recalled_by recall_kind`, p.messages, func(m messageRow) []any {
		return []any{m.Conversation, m.Seq, m.Sender, m.Kind, m.Body, m.Attachment, m.Duration,
			ptrIfPositive(m.Reply), m.Mentions, m.Created, m.Recalled, ptrIfPositive(m.RecalledBy), nonempty(m.RecallKind)}
	}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT seed_group_experience_from_history()`); err != nil {
		return destinationError("import group experience", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE conversation_members m
		SET level=group_level_from_points(coalesce((SELECT experience FROM group_member_experience e
			WHERE e.conversation_id=m.conversation_id AND e.user_id=m.user_id),0))
		FROM conversations c WHERE c.id=m.conversation_id AND c.kind='group' AND NOT m.level_custom`); err != nil {
		return destinationError("import group activity levels", err)
	}
	if err := copyRows(ctx, tx, "legacy_message_map", `source_table source_id conversation_id seq`, p.messages, func(m messageRow) []any {
		return []any{m.Table, m.SourceID, m.Conversation, m.Seq}
	}); err != nil {
		return err
	}
	return copyRows(ctx, tx, "featured_messages", `conversation_id seq added_by added_at`, p.featured, func(f featuredRow) []any {
		return []any{f.Conversation, f.Seq, f.Actor, f.Created}
	})
}

func (p *Plan) writeContent(ctx context.Context, tx pgx.Tx) error {
	if err := copyRows(ctx, tx, "space_posts", `id author_id parent_id reply_to_id content created_at`, p.posts, func(s postRow) []any {
		return []any{s.ID, s.Author, ptrIfPositive(s.Parent), ptrIfPositive(s.ReplyTo), s.Body, s.Created}
	}); err != nil {
		return err
	}
	if err := copyRows(ctx, tx, "space_post_media", `post_id position media_id`, p.postMedia, func(m postMediaRow) []any {
		return []any{m.Post, m.Position, m.Media}
	}); err != nil {
		return err
	}
	if err := copyRows(ctx, tx, "space_likes", `post_id user_id created_at`, p.likes, func(l likeRow) []any {
		return []any{l.Post, l.User, l.Created}
	}); err != nil {
		return err
	}
	if err := copyRows(ctx, tx, "group_file_folders", `id conversation_id name created_at`, p.folders, func(f folderRow) []any {
		return []any{f.ID, f.Room, f.Name, f.Created}
	}); err != nil {
		return err
	}
	return copyRows(ctx, tx, "group_files", `id conversation_id media_id uploader_id folder_id created_at`, p.groupFiles, func(f groupFileRow) []any {
		return []any{f.ID, f.Room, f.Media, f.Owner, ptrIfPositive(f.Folder), f.Created}
	})
}

func (p *Plan) restartIDs(ctx context.Context, tx pgx.Tx) error {
	for _, table := range []string{"accounts", "conversations", "friend_requests", "group_join_requests", "group_transfers",
		"notifications", "space_posts", "group_file_folders", "group_files", "reports"} {
		ident := pgx.Identifier{table}.Sanitize()
		var highest int64
		if err := tx.QueryRow(ctx, `SELECT coalesce(max(id),0) FROM `+ident).Scan(&highest); err != nil {
			return destinationError("find maximum ID for "+table, err)
		}
		if table == "conversations" {
			highest = max(highest, p.maxRoom)
		}
		if table == "notifications" {
			highest = max(highest, p.maxNotice)
		}
		highest = max(highest, p.legacyMaxID[table])
		if highest == math.MaxInt64 {
			return fmt.Errorf("destination %s has no remaining identity IDs", table)
		}
		// RESTART is transactional, including for identities. setval is not.
		if _, err := tx.Exec(ctx, `ALTER TABLE `+ident+` ALTER COLUMN id RESTART WITH `+strconv.FormatInt(highest+1, 10)); err != nil {
			return destinationError("restart identity for "+table, err)
		}
	}
	return nil
}

func (p *Plan) verifyDestination(ctx context.Context, tx pgx.Tx) error {
	for _, table := range importTables {
		var count int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&count); err != nil {
			return destinationError("verify "+table, err)
		}
		if count != p.report.Planned[table] {
			return formatRowsMismatch(table, count, p.report.Planned[table])
		}
	}
	var invalid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		WITH timelines AS (SELECT conversation_id, max(seq) AS last, count(*) AS total FROM messages GROUP BY conversation_id)
		SELECT 1 FROM conversation_heads h LEFT JOIN timelines t USING(conversation_id)
		WHERE h.head <> coalesce(t.last,0) OR h.head <> coalesce(t.total,0)
		UNION ALL
		SELECT 1 FROM conversation_members cm JOIN conversation_heads h USING(conversation_id) WHERE cm.read_seq > h.head
	)`).Scan(&invalid); err != nil {
		return destinationError("verify conversation sequences", err)
	}
	if invalid {
		return errors.New("destination conversation sequence validation failed")
	}
	return nil
}

func (p *Plan) storedKeys(ctx context.Context, tx pgx.Tx) ([]string, error) {
	if len(p.media) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, stored_key FROM media_objects`)
	if err != nil {
		return nil, destinationError("read media storage keys", err)
	}
	keys, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct {
		ID  pgtype.UUID
		Key string
	}, error) {
		var value struct {
			ID  pgtype.UUID
			Key string
		}
		err := row.Scan(&value.ID, &value.Key)
		return value, err
	})
	if err != nil {
		return nil, destinationError("read media storage keys", err)
	}
	store := make(map[pgtype.UUID]string, len(keys))
	for _, k := range keys {
		store[k.ID] = k.Key
	}
	ordered := make([]string, len(p.media))
	for i, m := range p.media {
		if key := store[m.ID]; key != "" {
			ordered[i] = key
		} else {
			return nil, errors.New("destination media storage key is missing")
		}
	}
	return ordered, nil
}

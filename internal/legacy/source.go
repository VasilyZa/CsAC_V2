// Package legacy inventories a quiesced CsAC v1 database and imports a reviewed
// snapshot into a fresh CsAC v2 PostgreSQL schema.
package legacy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"strings"

	"github.com/go-sql-driver/mysql"
)

const (
	maxSourceRows  = 2_000_000
	maxSourceBytes = 512 << 20
)

type sourceUser struct {
	ID, Created, Status, BanUntil, Deleted, FirstLogin, AutoJoin, Bot, LastActive int64
	Username, Nickname, Password, Email, Avatar, Hide, Pat, BanReason             string
}

type sourceRoom struct {
	ID, Owner, Listed, Search, JoinType, AllowInvite, Disbanded, DisbandTime, BanUntil, TransferCooldown int64
	Title, Intro, Notice, Code, FixedCode, Question, Avatar, Folders, BanReason                          string
}

type sourceMember struct {
	ID, Room, User, Mute, Read, Level, LevelCustom, TitleCustom int64
	Joined, Title                                               string
}

type sourceAdmin struct{ ID, Room, User int64 }
type sourceGrant struct{ User int64 }

type sourceFriend struct {
	ID, First, Second, Status, Actor, Read      int64
	Remark1, Remark2, Created, Updated, Deleted string
}

type sourceFriendRequest struct {
	ID, From, To, Type, Status int64
	Message, Created, Handled  string
}

type sourceGroupRequest struct {
	ID, Room, User, Type, Status int64
	Answer, Created              string
}

type sourceTransfer struct {
	ID, Room, From, To, Status int64
	Created                    string
}

type sourceGroupMessage struct {
	ID, Room, Sender, Kind, Recall, Reply, Duration, Featured int64
	Body, Created, Mentions, FileMeta                         string
}

type sourcePrivateMessage struct {
	ID, From, To, Room, Kind, Recall, Reply, Duration, Read, Created int64
	Type, Body, Image, Voice, FileMeta                               string
}

type sourceFeature struct {
	ID, Room, Message, Actor int64
	Created                  string
}

type sourceNotice struct {
	ID, User, Read             int64
	Title, Body, Link, Created string
}

type sourcePost struct {
	ID, Author, Reply, Parent, LikeCount int64
	Body, Images, Likes, Created         string
}

type sourceFile struct {
	ID, Room, Owner, Created, Expiry, Size int64
	Name, Folder, Path                     string
}

type sourceReport struct {
	ID, Reporter, Target, Anonymous, Created, Processed int64
	Kind, Name, Reason, Status, Reply                   string
}
type sourceSticker struct{ Code, Label, Path string }

type snapshot struct {
	users           []sourceUser
	grants          []sourceGrant
	rooms           []sourceRoom
	members         []sourceMember
	admins          []sourceAdmin
	friends         []sourceFriend
	friendRequests  []sourceFriendRequest
	groupRequests   []sourceGroupRequest
	transfers       []sourceTransfer
	groupMessages   []sourceGroupMessage
	privateMessages []sourcePrivateMessage
	features        []sourceFeature
	notices         []sourceNotice
	posts           []sourcePost
	files           []sourceFile
	stickers        []sourceSticker
	reports         []sourceReport
	tables          []TableReview
	digest          string
}

type snapshotReader struct {
	tx     *sql.Tx
	index  map[string]*TableReview
	digest hash.Hash
	rows   int64
	bytes  int64
}

func capture(ctx context.Context, dsn string) (*snapshot, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || cfg.DBName == "" || cfg.User == "" {
		return nil, errors.New("invalid MariaDB DSN: a named database and user are required")
	}
	// Select fixed columns using a known character set. Disable DSN features that
	// can issue arbitrary session commands, local file reads or multi-statements.
	cfg.Params = nil
	if err := cfg.Apply(mysql.Charset("utf8mb4", "")); err != nil {
		return nil, errors.New("invalid MariaDB connection charset")
	}
	cfg.MultiStatements = false
	cfg.AllowAllFiles = false
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, errors.New("invalid MariaDB connection configuration")
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return nil, sourceError("start read-only transaction", err)
	}
	defer tx.Rollback()

	s := &snapshot{}
	catalog, err := tx.QueryContext(ctx, `SELECT TABLE_NAME, COALESCE(ENGINE,''), TABLE_TYPE
		FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() ORDER BY BINARY TABLE_NAME`)
	if err != nil {
		return nil, sourceError("inventory tables", err)
	}
	for catalog.Next() {
		var table TableReview
		var kind string
		if err := catalog.Scan(&table.Name, &table.Engine, &kind); err != nil {
			catalog.Close()
			return nil, sourceError("decode table inventory", err)
		}
		table.Disposition = "out_of_scope"
		if kind != "BASE TABLE" {
			table.Disposition = "not_a_base_table"
		}
		s.tables = append(s.tables, table)
	}
	if err := catalog.Err(); err != nil {
		return nil, sourceError("read table inventory", err)
	}
	catalog.Close()

	r := &snapshotReader{tx: tx, index: make(map[string]*TableReview), digest: sha256.New()}
	for i := range s.tables {
		r.index[s.tables[i].Name] = &s.tables[i]
	}
	if err := s.read(ctx, r); err != nil {
		return nil, err
	}
	for i := range s.tables {
		table := &s.tables[i]
		if table.Disposition == "out_of_scope" {
			ident := "`" + strings.ReplaceAll(table.Name, "`", "``") + "`"
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+ident).Scan(&table.Rows); err != nil {
				return nil, sourceError("count an excluded table", err)
			}
		}
	}
	inventory, err := json.Marshal(s.tables)
	if err != nil {
		return nil, err
	}
	r.digest.Write(inventory)
	s.digest = hex.EncodeToString(r.digest.Sum(nil))
	return s, nil
}

func sourceError(action string, err error) error {
	// Driver errors can contain a DSN, a raw field from Scan, or server text.
	// Retain the phase and error number without printing those values.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("MariaDB %s: %w", action, err)
	}
	var server *mysql.MySQLError
	if errors.As(err, &server) {
		return fmt.Errorf("MariaDB %s failed (error %d)", action, server.Number)
	}
	return fmt.Errorf("MariaDB %s failed (%T)", action, err)
}

func readSource[T any](ctx context.Context, r *snapshotReader, out *[]T, name string, required bool, query string, fields func(*T) []any) error {
	table := r.index[name]
	if table == nil {
		if required {
			return fmt.Errorf("required MariaDB table %s is absent", name)
		}
		return nil
	}
	if table.Disposition == "not_a_base_table" {
		return fmt.Errorf("MariaDB %s must be a base table", name)
	}
	table.Disposition = "mapped"
	r.digest.Write([]byte(name + "\n"))
	rows, err := r.tx.QueryContext(ctx, query)
	if err != nil {
		return sourceError("query "+name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry T
		if err := rows.Scan(fields(&entry)...); err != nil {
			return sourceError("decode "+name, err)
		}
		payload, err := json.Marshal(entry)
		if err != nil {
			return sourceError("fingerprint "+name, err)
		}
		r.rows++
		r.bytes += int64(len(payload))
		if r.rows > maxSourceRows || r.bytes > maxSourceBytes {
			return fmt.Errorf("source snapshot exceeds the in-memory limit (%d rows or %d MiB)", maxSourceRows, maxSourceBytes>>20)
		}
		r.digest.Write(payload)
		r.digest.Write([]byte{'\n'})
		*out = append(*out, entry)
		table.Rows++
	}
	if err := rows.Err(); err != nil {
		return sourceError("read "+name, err)
	}
	return nil
}

func (s *snapshot) read(ctx context.Context, r *snapshotReader) error {
	if err := readSource(ctx, r, &s.users, "chat_user", true, `SELECT id,username,nickname,pwd,add_time,status,
		COALESCE(ban_until,0),delete_time,COALESCE(is_first_login,0),allow_auto_join,is_bot,COALESCE(email,''),
		COALESCE(avatar,''),hide_conv,pat_action,COALESCE(ban_reason,''),COALESCE(last_active,0) FROM chat_user ORDER BY id`, func(v *sourceUser) []any {
		return []any{&v.ID, &v.Username, &v.Nickname, &v.Password, &v.Created, &v.Status, &v.BanUntil, &v.Deleted, &v.FirstLogin, &v.AutoJoin, &v.Bot, &v.Email, &v.Avatar, &v.Hide, &v.Pat, &v.BanReason, &v.LastActive}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.grants, "csac_registration_code_redemptions", false,
		`SELECT uid FROM csac_registration_code_redemptions ORDER BY uid`, func(v *sourceGrant) []any {
			return []any{&v.User}
		}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.rooms, "chat_room", true, `SELECT id,owner_uid,room_name,COALESCE(intro,''),COALESCE(notice,''),
		invite_code,COALESCE(fixed_code,''),COALESCE(ask_question,''),COALESCE(avatar,''),file_groups,show_in_list,allow_search,
		COALESCE(join_type,0),COALESCE(allow_invite,1),COALESCE(is_disband,0),COALESCE(disband_time,0),COALESCE(ban_until,0),
		COALESCE(owner_transfer_cd,0),COALESCE(ban_reason,'') FROM chat_room ORDER BY id`, func(v *sourceRoom) []any {
		return []any{&v.ID, &v.Owner, &v.Title, &v.Intro, &v.Notice, &v.Code, &v.FixedCode, &v.Question, &v.Avatar, &v.Folders, &v.Listed, &v.Search, &v.JoinType, &v.AllowInvite, &v.Disbanded, &v.DisbandTime, &v.BanUntil, &v.TransferCooldown, &v.BanReason}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.members, "chat_group_user", true, `SELECT id,room_id,uid,CAST(join_time AS CHAR),
		COALESCE(mute_until,0),COALESCE(last_read_msg_id,0),title,level,COALESCE(level_custom,0),COALESCE(title_custom,0) FROM chat_group_user ORDER BY id`, func(v *sourceMember) []any {
		return []any{&v.ID, &v.Room, &v.User, &v.Joined, &v.Mute, &v.Read, &v.Title, &v.Level, &v.LevelCustom, &v.TitleCustom}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.admins, "chat_group_admin", true, `SELECT id,room_id,uid FROM chat_group_admin ORDER BY id`, func(v *sourceAdmin) []any {
		return []any{&v.ID, &v.Room, &v.User}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.friends, "friend_relation", true, `SELECT id,uid1,uid2,status,COALESCE(NULLIF(delete_by,0),deleted_by,0),
		COALESCE(remark1,''),COALESCE(remark2,''),CAST(create_time AS CHAR),CAST(update_time AS CHAR),COALESCE(CAST(delete_time AS CHAR),''),
		notice_read FROM friend_relation ORDER BY id`, func(v *sourceFriend) []any {
		return []any{&v.ID, &v.First, &v.Second, &v.Status, &v.Actor, &v.Remark1, &v.Remark2, &v.Created, &v.Updated, &v.Deleted, &v.Read}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.friendRequests, "friend_request", false, `SELECT id,from_uid,to_uid,type,status,COALESCE(content,''),
		CAST(create_time AS CHAR),COALESCE(CAST(handle_time AS CHAR),'') FROM friend_request ORDER BY id`, func(v *sourceFriendRequest) []any {
		return []any{&v.ID, &v.From, &v.To, &v.Type, &v.Status, &v.Message, &v.Created, &v.Handled}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.groupRequests, "chat_room_apply", false, `SELECT id,room_id,uid,apply_type,COALESCE(status,-1),
		COALESCE(answer_content,''),CAST(apply_time AS CHAR) FROM chat_room_apply ORDER BY id`, func(v *sourceGroupRequest) []any {
		return []any{&v.ID, &v.Room, &v.User, &v.Type, &v.Status, &v.Answer, &v.Created}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.transfers, "chat_room_transfer", false, `SELECT id,room_id,old_owner,new_owner,COALESCE(status,-1),
		CAST(create_time AS CHAR) FROM chat_room_transfer ORDER BY id`, func(v *sourceTransfer) []any {
		return []any{&v.ID, &v.Room, &v.From, &v.To, &v.Status, &v.Created}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.groupMessages, "chat_msg", true, `SELECT id,room_id,uid,COALESCE(msg_type,1),was_replied,
		COALESCE(reply_to,0),COALESCE(voice_duration,0),is_essence,COALESCE(content,''),CAST(add_time AS CHAR),
		COALESCE(mention_uids,''),COALESCE(file_meta,'') FROM chat_msg ORDER BY id`, func(v *sourceGroupMessage) []any {
		return []any{&v.ID, &v.Room, &v.Sender, &v.Kind, &v.Recall, &v.Reply, &v.Duration, &v.Featured, &v.Body, &v.Created, &v.Mentions, &v.FileMeta}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.privateMessages, "private_msg", true, `SELECT id,from_uid,to_uid,room_id,msg_type,
		COALESCE(is_recalled,0),COALESCE(reply_to,0),COALESCE(duration,0),is_read,created_at,type,COALESCE(content,''),
		COALESCE(image_url,''),COALESCE(voice_url,''),COALESCE(file_meta,'') FROM private_msg ORDER BY id`, func(v *sourcePrivateMessage) []any {
		return []any{&v.ID, &v.From, &v.To, &v.Room, &v.Kind, &v.Recall, &v.Reply, &v.Duration, &v.Read, &v.Created, &v.Type, &v.Body, &v.Image, &v.Voice, &v.FileMeta}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.features, "chat_essence", false, `SELECT id,room_id,msg_id,set_uid,CAST(set_time AS CHAR)
		FROM chat_essence ORDER BY id`, func(v *sourceFeature) []any {
		return []any{&v.ID, &v.Room, &v.Message, &v.Actor, &v.Created}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.notices, "chat_user_notice", true, `SELECT id,uid,COALESCE(is_read,0),COALESCE(title,''),
		COALESCE(content,''),COALESCE(link,''),CAST(add_time AS CHAR) FROM chat_user_notice ORDER BY id`, func(v *sourceNotice) []any {
		return []any{&v.ID, &v.User, &v.Read, &v.Title, &v.Body, &v.Link, &v.Created}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.posts, "space_shc", false, `SELECT cont_id,sender_uid,is_reply,COALESCE(reply_id,0),likes_num,
		content,COALESCE(img_conts,''),likes_uid,CAST(created_at AS CHAR) FROM space_shc ORDER BY cont_id`, func(v *sourcePost) []any {
		return []any{&v.ID, &v.Author, &v.Reply, &v.Parent, &v.LikeCount, &v.Body, &v.Images, &v.Likes, &v.Created}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.files, "group_files", false, `SELECT id,room_id,uploader_uid,created_at,expires_at,file_size,
		file_name,file_group,file_path FROM group_files ORDER BY id`, func(v *sourceFile) []any {
		return []any{&v.ID, &v.Room, &v.Owner, &v.Created, &v.Expiry, &v.Size, &v.Name, &v.Folder, &v.Path}
	}); err != nil {
		return err
	}
	if err := readSource(ctx, r, &s.reports, "chat_report", false, `SELECT id,reporter_uid,target_id,COALESCE(is_anonymous,0),add_time,
		COALESCE(process_time,0),report_type,COALESCE(target_name,''),reason,COALESCE(status,''),COALESCE(admin_reply,'')
		FROM chat_report ORDER BY id`, func(v *sourceReport) []any {
		return []any{&v.ID, &v.Reporter, &v.Target, &v.Anonymous, &v.Created, &v.Processed, &v.Kind, &v.Name, &v.Reason, &v.Status, &v.Reply}
	}); err != nil {
		return err
	}
	return readSource(ctx, r, &s.stickers, "emoji_list", false, `SELECT abbr,COALESCE(full_name,''),COALESCE(address,'')
		FROM emoji_list ORDER BY BINARY abbr`, func(v *sourceSticker) []any {
		return []any{&v.Code, &v.Label, &v.Path}
	})
}

package media

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"csac/v2/internal/rpc"
	"github.com/jackc/pgx/v5"
)

func registerGroupFiles(reg *rpc.Registry) {
	reg.Register("group.file.list", rpc.Method{Access: rpc.Member, Handle: listGroupFiles})
	reg.Register("group.file.add", rpc.Method{Access: rpc.Member, Write: true, Dedup: true, Handle: addGroupFile})
	reg.Register("group.file.remove", rpc.Method{Access: rpc.Member, Write: true, Dedup: true, Handle: removeGroupFile})
	reg.Register("group.file.move", rpc.Method{Access: rpc.Member, Write: true, Dedup: true, Handle: moveGroupFile})
	reg.Register("group.file.folder", rpc.Method{Access: rpc.Member, Write: true, Dedup: true, Handle: changeFolder})
}

type groupFolder struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type groupFile struct {
	ID           int64      `json:"id"`
	MediaID      string     `json:"media_id"`
	FolderID     *int64     `json:"folder_id"`
	UploaderID   int64      `json:"uploader_id"`
	UploaderName string     `json:"uploader_name"`
	Filename     string     `json:"filename"`
	MIME         string     `json:"mime"`
	Size         int64      `json:"size"`
	ExpiresAt    *time.Time `json:"expires_at"`
	CreatedAt    time.Time  `json:"created_at"`
	DownloadPath string     `json:"download_path"`
}

type groupList struct {
	Files   []groupFile   `json:"files"`
	Folders []groupFolder `json:"folders"`
	Before  int64         `json:"next_before_id,omitempty"`
}

func listGroupFiles(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		ConversationID int64  `json:"conversation_id"`
		FolderID       *int64 `json:"folder_id"`
		Before         int64  `json:"before_id"`
		Limit          int    `json:"limit"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.Limit == 0 {
		in.Limit = 50
	}
	if in.ConversationID <= 0 || in.Before < 0 || in.Limit < 1 || in.Limit > 100 || (in.FolderID != nil && *in.FolderID < 0) {
		return nil, rpc.Invalid("Invalid group file page")
	}
	var out groupList
	err = call.DB.QueryRow(ctx, `WITH visible AS (
			SELECT c.id FROM conversations c JOIN conversation_members cm ON cm.conversation_id=c.id
			WHERE c.id=$1 AND cm.user_id=$2 AND c.kind='group' AND c.archived_at IS NULL
		), files AS (
			SELECT f.id, f.media_id::text AS media_id, f.folder_id, f.uploader_id, a.nickname AS uploader_name,
			       m.filename, m.mime, m.size, m.expires_at, f.created_at
			FROM visible v JOIN group_files f ON f.conversation_id=v.id
			JOIN media_objects m ON m.id=f.media_id
			JOIN accounts a ON a.id=f.uploader_id
			WHERE m.purpose='group' AND `+readyPredicate+` AND ($3::bigint IS NULL OR f.folder_id IS NOT DISTINCT FROM nullif($3,0))
			AND ($4=0 OR f.id<$4) ORDER BY f.id DESC LIMIT $5
		)
		SELECT coalesce((SELECT jsonb_agg(f ORDER BY f.id DESC) FROM files f),'[]'::jsonb),
		       coalesce((SELECT jsonb_agg(jsonb_build_object('id',id,'name',name) ORDER BY id)
			         FROM group_file_folders WHERE conversation_id=v.id),'[]'::jsonb)
		FROM visible v`, in.ConversationID, call.Actor.ID, in.FolderID, in.Before, in.Limit+1).Scan(&out.Files, &out.Folders)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, rpc.NotFound("Group unavailable")
	}
	if err != nil {
		return nil, err
	}
	if len(out.Files) > in.Limit {
		out.Files = out.Files[:in.Limit]
		out.Before = out.Files[in.Limit-1].ID
	}
	for i := range out.Files {
		out.Files[i].DownloadPath = mediaPath + out.Files[i].MediaID
	}
	return out, nil
}

type fileInput struct {
	ConversationID int64 `json:"conversation_id"`
	FolderID       int64 `json:"folder_id"`
}

func addGroupFile(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		fileInput
		MediaID string `json:"media_id"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if _, err := lockGroup(ctx, call, in.ConversationID); err != nil {
		return nil, err
	}
	if err := requireFolder(ctx, call.DB, in.fileInput); err != nil {
		return nil, err
	}
	if err := RequireOwnedReady(ctx, call.DB, call.Actor.ID, in.MediaID, "group"); err != nil {
		return nil, err
	}
	var out idResult
	err = call.DB.QueryRow(ctx, `INSERT INTO group_files(conversation_id,media_id,uploader_id,folder_id)
		VALUES ($1,$2::uuid,$3,nullif($4::bigint,0)) RETURNING id`, in.ConversationID, in.MediaID, call.Actor.ID, in.FolderID).Scan(&out.ID)
	if err != nil {
		return nil, err
	}
	return out, emitGroupChange(ctx, call, in.ConversationID)
}

func removeGroupFile(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		ConversationID int64 `json:"conversation_id"`
		FileID         int64 `json:"file_id"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if err := authorizeFileChange(ctx, call, in.ConversationID, in.FileID, true); err != nil {
		return nil, err
	}
	if _, err := call.DB.Exec(ctx, `DELETE FROM group_files WHERE id=$1 AND conversation_id=$2`, in.FileID, in.ConversationID); err != nil {
		return nil, err
	}
	return idResult{in.FileID}, emitGroupChange(ctx, call, in.ConversationID)
}

func moveGroupFile(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		fileInput
		FileID int64 `json:"file_id"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if err := authorizeFileChange(ctx, call, in.ConversationID, in.FileID, false); err != nil {
		return nil, err
	}
	if err := requireFolder(ctx, call.DB, in.fileInput); err != nil {
		return nil, err
	}
	if _, err := call.DB.Exec(ctx, `UPDATE group_files SET folder_id=nullif($3::bigint,0) WHERE id=$1 AND conversation_id=$2`,
		in.FileID, in.ConversationID, in.FolderID); err != nil {
		return nil, err
	}
	return idResult{in.FileID}, emitGroupChange(ctx, call, in.ConversationID)
}

type idResult struct {
	ID int64 `json:"id"`
}

func changeFolder(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		fileInput
		Action string `json:"action"`
		Name   string `json:"name"`
	}](raw)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	switch in.Action {
	case "create", "rename":
		if name == "" || utf8.RuneCountInString(name) > 50 || strings.IndexFunc(name, unicode.IsControl) != -1 {
			return nil, rpc.Invalid("Folder name must contain 1 to 50 characters")
		}
	case "delete":
		if name != "" {
			return nil, rpc.Invalid("Deleting a folder only requires its ID")
		}
	default:
		return nil, rpc.Invalid("Invalid folder action")
	}
	if (in.Action == "create" && in.FolderID != 0) || (in.Action != "create" && in.FolderID <= 0) {
		return nil, rpc.Invalid("Invalid folder ID")
	}
	role, err := lockGroup(ctx, call, in.ConversationID)
	if err != nil {
		return nil, err
	}
	if !managesGroup(role) {
		return nil, rpc.Denied("Group owner or administrator required")
	}
	var out idResult
	switch in.Action {
	case "create":
		err = call.DB.QueryRow(ctx, `INSERT INTO group_file_folders(conversation_id,name)
			SELECT $1,$2 WHERE (SELECT count(*) FROM group_file_folders WHERE conversation_id=$1)<200 RETURNING id`,
			in.ConversationID, name).Scan(&out.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, rpc.Conflict("Group has reached its folder limit")
		}
	case "rename":
		err = call.DB.QueryRow(ctx, `UPDATE group_file_folders SET name=$3 WHERE id=$1 AND conversation_id=$2 RETURNING id`,
			in.FolderID, in.ConversationID, name).Scan(&out.ID)
	case "delete":
		err = call.DB.QueryRow(ctx, `DELETE FROM group_file_folders WHERE id=$1 AND conversation_id=$2 RETURNING id`,
			in.FolderID, in.ConversationID).Scan(&out.ID)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, rpc.NotFound("Folder unavailable")
	}
	if err != nil {
		return nil, err
	}
	return out, emitGroupChange(ctx, call, in.ConversationID)
}

func lockGroup(ctx context.Context, call rpc.Call, id int64) (string, error) {
	role, err := lockGroupRole(ctx, call, id)
	if err == nil && role == "" {
		return "", rpc.NotFound("Group unavailable")
	}
	return role, err
}

func lockGroupRole(ctx context.Context, call rpc.Call, id int64) (string, error) {
	if id <= 0 {
		return "", rpc.Invalid("Expected a conversation ID")
	}
	var exists bool
	err := call.DB.QueryRow(ctx, `SELECT true FROM conversations WHERE id=$1 AND kind='group' AND archived_at IS NULL FOR UPDATE`, id).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", rpc.NotFound("Group unavailable")
	}
	if err != nil {
		return "", err
	}
	// Read membership after obtaining the lock. Kicks and departures use the same group lock.
	var role string
	err = call.DB.QueryRow(ctx, `SELECT role FROM conversation_members WHERE conversation_id=$1 AND user_id=$2 FOR SHARE`,
		id, call.Actor.ID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return role, err
}

func requireFolder(ctx context.Context, db rpc.DB, in fileInput) error {
	if in.FolderID < 0 {
		return rpc.Invalid("Invalid folder ID")
	}
	if in.FolderID == 0 {
		return nil
	}
	var ok bool
	err := db.QueryRow(ctx, `SELECT true FROM group_file_folders WHERE id=$1 AND conversation_id=$2`, in.FolderID, in.ConversationID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return rpc.NotFound("Folder unavailable")
	}
	return err
}

func authorizeFileChange(ctx context.Context, call rpc.Call, conversationID, fileID int64, withdrawOwn bool) error {
	if fileID <= 0 {
		return rpc.Invalid("Expected a file ID")
	}
	role, err := lockGroupRole(ctx, call, conversationID)
	if err != nil {
		return err
	}
	if role == "" && !withdrawOwn {
		return rpc.NotFound("Group unavailable")
	}
	var uploaderID int64
	err = call.DB.QueryRow(ctx, `SELECT f.uploader_id FROM group_files f JOIN media_objects m ON m.id=f.media_id
		WHERE f.id=$1 AND f.conversation_id=$2 AND ($3::boolean OR f.uploader_id=$4) AND `+readyPredicate+` FOR UPDATE OF f`,
		fileID, conversationID, role != "", call.Actor.ID).Scan(&uploaderID)
	if errors.Is(err, pgx.ErrNoRows) {
		return rpc.NotFound("Group file unavailable")
	}
	if err != nil {
		return err
	}
	if call.Actor.ID != uploaderID && !managesGroup(role) {
		return rpc.Denied("Only the uploader or a group manager may change this file")
	}
	return nil
}

func managesGroup(role string) bool { return role == "owner" || role == "admin" }

func emitGroupChange(ctx context.Context, call rpc.Call, conversationID int64) error {
	rows, err := call.DB.Query(ctx, `SELECT cm.user_id FROM conversation_members cm JOIN accounts a ON a.id=cm.user_id
		WHERE cm.conversation_id=$1 AND a.status='active'`, conversationID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	return call.Emit("group.files_changed", ids, struct{}{})
}

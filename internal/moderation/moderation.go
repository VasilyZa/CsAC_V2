package moderation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"csac/v2/internal/rpc"
	"github.com/jackc/pgx/v5"
)

func Register(r *rpc.Registry) {
	r.Register("reports.submit", rpc.Method{Access: rpc.Member, Write: true, Dedup: true, Handle: submit})
	r.Register("reports.bug", rpc.Method{Access: rpc.Member, Write: true, Dedup: true, Handle: bug})
	r.Register("admin.reports.list", rpc.Method{Access: rpc.Admin, Handle: list})
	r.Register("admin.reports.review", rpc.Method{Access: rpc.Admin, Write: true, Dedup: true, Handle: review})
	r.Register("admin.group.ban", rpc.Method{Access: rpc.Admin, Write: true, Dedup: true, Handle: banGroup})
	r.Register("admin.group.unban", rpc.Method{Access: rpc.Admin, Write: true, Dedup: true, Handle: unbanGroup})
	r.Register("admin.bans.list", rpc.Method{Access: rpc.Admin, Handle: listBans})
}

func submit(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		Kind      string `json:"target_kind"`
		Target    int64  `json:"target_id"`
		Reason    string `json:"reason"`
		Anonymous bool   `json:"anonymous"`
	}](raw)
	if err != nil {
		return nil, err
	}
	in.Reason = strings.TrimSpace(in.Reason)
	if utf8.RuneCountInString(in.Reason) < 10 || len(in.Reason) > 12000 || in.Target <= 0 {
		return nil, rpc.Invalid("Provide a target and a reason of at least 10 characters")
	}
	var targetName string
	switch in.Kind {
	case "user":
		err = call.DB.QueryRow(ctx, `SELECT nickname FROM accounts WHERE id=$1 AND status<>'deleted'`, in.Target).Scan(&targetName)
	case "group":
		err = call.DB.QueryRow(ctx, `SELECT title FROM conversations WHERE id=$1 AND kind='group'`, in.Target).Scan(&targetName)
	default:
		return nil, rpc.Invalid("Unsupported report target")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, rpc.NotFound("Report target unavailable")
	}
	if err != nil {
		return nil, err
	}
	return save(ctx, call, in.Kind, &in.Target, targetName, in.Reason, in.Anonymous)
}
func bug(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}](raw)
	if err != nil {
		return nil, err
	}
	in.Title, in.Description = strings.TrimSpace(in.Title), strings.TrimSpace(in.Description)
	if in.Title == "" || len(in.Title) > 256 || in.Description == "" || len(in.Description) > 12000 {
		return nil, rpc.Invalid("Supply a title and description within the limits")
	}
	return save(ctx, call, "bug", nil, in.Title, in.Description, false)
}

type reportRef struct {
	ID int64 `json:"report_id"`
}

func save(ctx context.Context, call rpc.Call, kind string, target *int64, title, reason string, anonymous bool) (any, error) {
	var ref reportRef
	if err := call.DB.QueryRow(ctx, `INSERT INTO reports(reporter_id,target_kind,target_id,title,reason,anonymous)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, call.Actor.ID, kind, target, title, reason, anonymous).Scan(&ref.ID); err != nil {
		return nil, err
	}
	rows, err := call.DB.Query(ctx, `SELECT id FROM accounts WHERE site_admin AND status='active'`)
	if err != nil {
		return nil, err
	}
	admins, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(ref)
	if len(admins) > 0 {
		if _, err = call.DB.Exec(ctx, `INSERT INTO notifications(user_id,kind,data) SELECT unnest($1::bigint[]),'report.created',$2`, admins, payload); err != nil {
			return nil, err
		}
	}
	return ref, call.Emit("report.created", admins, ref)
}

type row struct {
	reportRef
	Reporter  *int64    `json:"reporter_id,omitempty"`
	Kind      string    `json:"target_kind"`
	Target    *int64    `json:"target_id,omitempty"`
	Title     string    `json:"title,omitempty"`
	Reason    string    `json:"reason"`
	Anonymous bool      `json:"anonymous"`
	State     string    `json:"state"`
	Note      string    `json:"note,omitempty"`
	Created   time.Time `json:"created_at"`
}

func list(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		Before int64  `json:"before_id"`
		Limit  int    `json:"limit"`
		State  string `json:"state"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.Limit == 0 {
		in.Limit = 20
	}
	if in.Limit <= 0 || in.Limit > 30 || in.Before < 0 || (in.State != "" && in.State != "pending" && in.State != "processing" && in.State != "resolved") {
		return nil, rpc.Invalid("Invalid report page")
	}
	if err := requireBanAdmin(ctx, call, false); err != nil {
		return nil, err
	}
	rows, err := call.DB.Query(ctx, `SELECT id,CASE WHEN NOT anonymous THEN reporter_id END,target_kind,target_id,title,reason,anonymous,state,action_note,created_at FROM reports
		WHERE ($1::bigint=0 OR id<$1) AND ($2='' OR state=$2) ORDER BY id DESC LIMIT $3`, in.Before, in.State, in.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]row, 0, in.Limit)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ID, &r.Reporter, &r.Kind, &r.Target, &r.Title, &r.Reason, &r.Anonymous, &r.State, &r.Note, &r.Created); err != nil {
			return nil, err
		}
		items = append(items, r)
	}
	return items, rows.Err()
}

func review(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		reportRef
		State string `json:"state"`
		Note  string `json:"note"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if in.ID <= 0 || len(in.Note) > 4000 || (in.State != "processing" && in.State != "resolved") {
		return nil, rpc.Invalid("Invalid review state")
	}
	if err := requireBanAdmin(ctx, call, true); err != nil {
		return nil, err
	}
	var reporter *int64
	if err := call.DB.QueryRow(ctx, `UPDATE reports SET state=$2,action_note=$3,reviewer_id=$4,updated_at=clock_timestamp() WHERE id=$1 RETURNING reporter_id`, in.ID, in.State, in.Note, call.Actor.ID).Scan(&reporter); err != nil {
		return nil, err
	}
	if reporter != nil {
		payload, _ := json.Marshal(in)
		if _, err := call.DB.Exec(ctx, `INSERT INTO notifications(user_id,kind,data) VALUES ($1,'report.reviewed',$2)`, *reporter, payload); err != nil {
			return nil, err
		}
		if err := call.Emit("report.reviewed", []int64{*reporter}, in); err != nil {
			return nil, err
		}
	}
	return in, nil
}

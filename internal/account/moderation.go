package account

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"csac/v2/internal/rpc"

	"github.com/jackc/pgx/v5"
)

type deleteResult struct {
	UserID            int64     `json:"user_id"`
	Status            string    `json:"status"`
	CoolingPeriodDays int       `json:"cooling_period_days"`
	RestorableUntil   time.Time `json:"restorable_until"`
}

func (s *Service) deleteAccount(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	if err := decodeEmpty(raw); err != nil {
		return nil, err
	}
	a, err := currentAccount(ctx, call, true)
	if err != nil {
		return nil, err
	}
	if a.siteAdmin {
		return nil, rpc.Denied("A site administrator cannot delete their account")
	}
	var deletedAt time.Time
	err = call.DB.QueryRow(ctx, `UPDATE accounts SET status='deleted',deleted_at=clock_timestamp(),ban_until=NULL,ban_reason=''
		WHERE id=$1 RETURNING deleted_at`, a.id).Scan(&deletedAt)
	if err != nil {
		return nil, err
	}
	if err := deactivateCredentials(ctx, call.DB, a.id); err != nil {
		return nil, err
	}
	return deleteResult{a.id, "deleted", coolingPeriodDays, deletedAt.Add(coolingPeriod)},
		emitAccount(call, "account.status_changed", accountChange{UserID: a.id, Status: "deleted"})
}

func deactivateCredentials(ctx context.Context, db rpc.DB, uid int64) error {
	if _, err := db.Exec(ctx, `DELETE FROM account_sessions WHERE user_id=$1`, uid); err != nil {
		return err
	}
	return invalidateChallenges(ctx, db, uid)
}

type restoreArgs struct {
	Email string `json:"email"`
	Token string `json:"restore_token"`
}

func (s *Service) restoreByEmail(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	args, err := rpc.Decode[restoreArgs](raw)
	if err != nil {
		return nil, err
	}
	address, err := normalizeEmail(args.Email)
	if err != nil {
		return nil, err
	}
	a, err := findAccount(ctx, call.DB, emailMatch, address, true)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (!a.emailVerified || !a.recoverable)) {
		return rejectedChallenge(), nil
	}
	if err != nil {
		return nil, err
	}
	verified, err := s.consumeChallenge(ctx, call.DB, challengeRestore, address, &a.id, args.Token)
	if err != nil {
		return nil, err
	}
	if !verified {
		return rejectedChallenge(), nil
	}
	return restoreAccount(ctx, call, a)
}

type targetArgs struct {
	UserID int64 `json:"user_id"`
}

type banArgs struct {
	UserID int64  `json:"user_id"`
	Days   int    `json:"ban_days"`
	Reason string `json:"reason"`
}

type banResult struct {
	accountChange
	BanUntil *time.Time `json:"ban_until"`
}

func (s *Service) ban(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	args, err := rpc.Decode[banArgs](raw)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(args.Reason)
	if args.Days < 0 || args.Days > 36500 || !validLabel(reason, 1, 500) {
		return nil, rpc.Invalid("Supply a ban reason and, optionally, a duration of 1 to 36500 days")
	}
	a, err := moderationTarget(ctx, call, args.UserID)
	if err != nil {
		return nil, err
	}
	if a.status == "deleted" {
		return nil, rpc.Conflict("Account is deleted")
	}
	var until *time.Time
	err = call.DB.QueryRow(ctx, `UPDATE accounts SET status='banned',ban_reason=$2,
		ban_until=CASE WHEN $3::integer=0 THEN NULL ELSE clock_timestamp()+$3 * interval '24 hours' END
		WHERE id=$1 RETURNING ban_until`, a.id, reason, args.Days).Scan(&until)
	if err != nil {
		return nil, err
	}
	if err := deactivateCredentials(ctx, call.DB, a.id); err != nil {
		return nil, err
	}
	change := accountChange{UserID: a.id, Status: "banned"}
	result := banResult{accountChange: change, BanUntil: until}
	if err := notifyAccountModeration(ctx, call, "account.banned", result, reason); err != nil {
		return nil, err
	}
	return result, emitAccount(call, "account.status_changed", change)
}

func (s *Service) unban(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	args, err := rpc.Decode[targetArgs](raw)
	if err != nil {
		return nil, err
	}
	a, err := moderationTarget(ctx, call, args.UserID)
	if err != nil {
		return nil, err
	}
	change := accountChange{UserID: a.id, Status: "active"}
	switch a.status {
	case "deleted":
		return nil, rpc.Conflict("Account is deleted")
	case "active":
		return change, nil
	}
	if _, err := call.DB.Exec(ctx, `UPDATE accounts SET status='active',ban_until=NULL,ban_reason='' WHERE id=$1`, a.id); err != nil {
		return nil, err
	}
	if err := notifyAccountModeration(ctx, call, "account.unbanned", banResult{accountChange: change}, ""); err != nil {
		return nil, err
	}
	return change, emitAccount(call, "account.status_changed", change)
}

// Persist the explanation before publishing the refresh. The surrounding RPC
// transaction commits the account, credential revocation, notice and events together.
func notifyAccountModeration(ctx context.Context, call rpc.Call, kind string, state banResult, reason string) error {
	payload := struct {
		banResult
		Reason string `json:"reason,omitempty"`
	}{state, reason}
	if _, err := call.DB.Exec(ctx, `INSERT INTO notifications(user_id,kind,data) VALUES ($1,$2,$3::jsonb)`, state.UserID, kind, payload); err != nil {
		return err
	}
	return call.Emit("notices.changed", []int64{state.UserID}, struct{}{})
}

func (s *Service) adminRestore(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	args, err := rpc.Decode[targetArgs](raw)
	if err != nil {
		return nil, err
	}
	a, err := moderationTarget(ctx, call, args.UserID)
	if err != nil {
		return nil, err
	}
	return restoreAccount(ctx, call, a)
}

func restoreAccount(ctx context.Context, call rpc.Call, a accountState) (any, error) {
	if !a.recoverable {
		return nil, rpc.Conflict("Account is outside the recovery window")
	}
	tag, err := call.DB.Exec(ctx, `UPDATE accounts SET status='active',deleted_at=NULL,ban_until=NULL,ban_reason='' WHERE id=$1
		AND status='deleted' AND purged_at IS NULL AND deleted_at > clock_timestamp()-$2::double precision*interval '1 second'`,
		a.id, coolingPeriod.Seconds())
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() != 1 {
		return nil, rpc.Conflict("Account is outside the recovery window")
	}
	if err := deactivateCredentials(ctx, call.DB, a.id); err != nil {
		return nil, err
	}
	change := accountChange{UserID: a.id, Status: "active"}
	return change, emitAccount(call, "account.status_changed", change)
}

func moderationTarget(ctx context.Context, call rpc.Call, uid int64) (accountState, error) {
	if uid <= 0 {
		return accountState{}, rpc.Invalid("Supply a valid user ID")
	}
	if err := requireAdmin(ctx, call, true); err != nil {
		return accountState{}, err
	}
	// Filter before locking so two administrators cannot lock each other's row.
	a, err := findAccount(ctx, call.DB, `id=$1 AND NOT site_admin`, uid, true)
	if errors.Is(err, pgx.ErrNoRows) {
		var siteAdmin bool
		if err := call.DB.QueryRow(ctx, `SELECT site_admin FROM accounts WHERE id=$1`, uid).Scan(&siteAdmin); err != nil {
			return accountState{}, err
		}
		if siteAdmin {
			return accountState{}, rpc.Denied("A site administrator cannot be moderated here")
		}
	}
	return a, err
}

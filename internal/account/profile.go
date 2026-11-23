package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"csac/v2/internal/rpc"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type publicProfile struct {
	UserID        int64     `json:"user_id"`
	Username      string    `json:"username"`
	Nickname      string    `json:"nickname"`
	AvatarID      *string   `json:"avatar_id"`
	Bio           string    `json:"bio"`
	AllowAutoJoin bool      `json:"allow_auto_join"`
	PatAction     string    `json:"pat_action"`
	CreatedAt     time.Time `json:"created_at"`
}

const profileColumns = `id,username,nickname,avatar_id::text,bio,allow_auto_join,pat_action,created_at`

func readProfile(row pgx.Row) (publicProfile, error) {
	var p publicProfile
	err := row.Scan(&p.UserID, &p.Username, &p.Nickname, &p.AvatarID, &p.Bio, &p.AllowAutoJoin, &p.PatAction, &p.CreatedAt)
	return p, err
}

type selfProfile struct {
	publicProfile
	Email                  *string `json:"email"`
	EmailVerified          bool    `json:"email_verified"`
	RegistrationGrant      bool    `json:"registration_grant"`
	SiteAdmin              bool    `json:"site_admin"`
	NeedsEmailVerification bool    `json:"needs_email_verification"`
}

func (s *Service) self(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	if err := decodeEmpty(raw); err != nil {
		return nil, err
	}
	var p selfProfile
	// The private mailbox is fetched only alongside this exact, active session.
	err := call.DB.QueryRow(ctx, `SELECT `+profileColumns+`,email,email_verified,registration_grant,site_admin
		FROM accounts WHERE id=$1 AND (status='active' OR (status='banned' AND ban_until<=clock_timestamp())) AND EXISTS
			(SELECT 1 FROM account_sessions s WHERE s.user_id=accounts.id AND s.token_hash=$2 AND s.expires_at>clock_timestamp())`,
		call.Actor.ID, call.Actor.Session).Scan(&p.UserID, &p.Username, &p.Nickname, &p.AvatarID, &p.Bio, &p.AllowAutoJoin,
		&p.PatAction, &p.CreatedAt, &p.Email, &p.EmailVerified, &p.RegistrationGrant, &p.SiteAdmin)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, rpc.Unauthenticated()
	}
	if err != nil {
		return nil, err
	}
	p.NeedsEmailVerification = s.needsEmail(p.EmailVerified, p.RegistrationGrant)
	return p, nil
}

type lookupArgs struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
}

func (s *Service) lookup(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	args, err := rpc.Decode[lookupArgs](raw)
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(args.Username)
	if args.UserID < 0 || len(name) > 255 || strings.ContainsRune(name, 0) || (args.UserID > 0) == (name != "") {
		return nil, rpc.Invalid("Supply either a user ID or a username")
	}
	predicate, value := idMatch, any(args.UserID)
	if name != "" {
		predicate, value = usernameMatch, name
	}
	return readProfile(call.DB.QueryRow(ctx, `SELECT `+profileColumns+` FROM accounts WHERE `+predicate+
		` AND (status='active' OR (status='banned' AND ban_until<=clock_timestamp()))`, value))
}

type optionalAvatar struct {
	set bool
	id  pgtype.UUID
}

func (a *optionalAvatar) UnmarshalJSON(raw []byte) error {
	a.set = true
	if bytes.Equal(raw, []byte("null")) {
		a.id = pgtype.UUID{}
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	if len(value) != 36 || a.id.Scan(value) != nil {
		return errors.New("avatar_id must be a UUID or null")
	}
	return nil
}

type profileArgs struct {
	Nickname      *string        `json:"nickname"`
	Bio           *string        `json:"bio"`
	AvatarID      optionalAvatar `json:"avatar_id"`
	AllowAutoJoin *bool          `json:"allow_auto_join"`
	PatAction     *string        `json:"pat_action"`
}

func (s *Service) updateProfile(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	args, err := rpc.Decode[profileArgs](raw)
	if err != nil {
		return nil, err
	}
	if args.Nickname == nil && args.Bio == nil && !args.AvatarID.set && args.AllowAutoJoin == nil && args.PatAction == nil {
		return nil, rpc.Invalid("No profile changes supplied")
	}
	if args.Nickname != nil {
		name := strings.TrimSpace(*args.Nickname)
		if !validLabel(name, 1, 16) || reservedName(name) {
			return nil, rpc.Invalid("Nickname must be 1 to 16 characters and must not be reserved")
		}
		args.Nickname = &name
	}
	if args.Bio != nil {
		bio := strings.TrimSpace(*args.Bio)
		if utf8.RuneCountInString(bio) > 500 || strings.ContainsFunc(bio, func(r rune) bool {
			return unicode.IsControl(r) && r != '\r' && r != '\n' && r != '\t'
		}) {
			return nil, rpc.Invalid("Bio must be at most 500 characters and contain only text")
		}
		args.Bio = &bio
	}
	if args.PatAction != nil {
		verb := strings.TrimSpace(*args.PatAction)
		if verb == "" {
			verb = defaultPatAction
		}
		if !validLabel(verb, 1, 16) {
			return nil, rpc.Invalid("Pat action must be at most 16 characters")
		}
		args.PatAction = &verb
	}
	a, err := currentAccount(ctx, call, true)
	if err != nil {
		return nil, err
	}
	if args.AvatarID.id.Valid {
		var exists bool
		err := call.DB.QueryRow(ctx, `SELECT true FROM media_objects WHERE id=$1 AND owner_id=$2 AND state='ready'
			AND purpose='avatar' AND purged_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp()) FOR SHARE`,
			args.AvatarID.id, a.id).Scan(&exists)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, rpc.Invalid("Select one of your ready avatar uploads")
		}
		if err != nil {
			return nil, err
		}
	}
	p, err := readProfile(call.DB.QueryRow(ctx, `UPDATE accounts SET nickname=coalesce($2,nickname), bio=coalesce($3,bio),
		avatar_id=CASE WHEN $4::boolean THEN $5::uuid ELSE avatar_id END,
		allow_auto_join=coalesce($6,allow_auto_join), pat_action=coalesce($7,pat_action) WHERE id=$1 RETURNING `+profileColumns,
		a.id, args.Nickname, args.Bio, args.AvatarID.set, args.AvatarID.id, args.AllowAutoJoin, args.PatAction))
	if err != nil {
		return nil, err
	}
	return p, emitAccount(call, "profile.updated", accountChange{UserID: a.id})
}

type passwordArgs struct {
	Old string `json:"old_password"`
	New string `json:"new_password"`
}

type updatedResult struct {
	Updated bool `json:"updated"`
}

func (s *Service) preparePasswordChange(ctx context.Context, actor rpc.Actor, _ string, raw json.RawMessage) (rpc.PreparedHandler, func(), error) {
	args, err := rpc.Decode[passwordArgs](raw)
	if err != nil {
		return nil, nil, err
	}
	if args.Old == "" {
		return nil, nil, rpc.Invalid("Supply the current password")
	}
	if err := validateNewPassword(args.New); err != nil {
		return nil, nil, err
	}
	if !s.authRates.passwordChange(actor.ID) {
		return nil, nil, authRateExceeded()
	}
	release, err := s.admitPasswordWork(ctx)
	if err != nil {
		return nil, nil, err
	}
	return func(ctx context.Context, call rpc.Call) (any, error) {
		return s.changePassword(ctx, call, args)
	}, release, nil
}

func (s *Service) changePassword(ctx context.Context, call rpc.Call, args passwordArgs) (any, error) {
	a, err := currentAccount(ctx, call, true)
	if err != nil {
		return nil, err
	}
	verified, _, err := s.verifyPassword(ctx, a.passwordHash, passwordForCredential(a.passwordHash, args.Old), a.username)
	if err != nil {
		return nil, err
	}
	if !verified {
		return nil, invalidCredentials()
	}
	hash, err := s.hashPassword(ctx, args.New)
	if err != nil {
		return nil, err
	}
	if _, err := call.DB.Exec(ctx, `UPDATE accounts SET password_hash=$2 WHERE id=$1`, a.id, hash); err != nil {
		return nil, err
	}
	if _, err := call.DB.Exec(ctx, `DELETE FROM account_sessions WHERE user_id=$1 AND token_hash<>$2`, a.id, call.Actor.Session); err != nil {
		return nil, err
	}
	if err := invalidateChallenges(ctx, call.DB, a.id); err != nil {
		return nil, err
	}
	return updatedResult{true}, emitAccount(call, "account.password_changed", accountChange{UserID: a.id})
}

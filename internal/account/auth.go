package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"csac/v2/internal/rpc"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_@.\-]{3,32}$`)
var reservedNames = map[string]struct{}{
	"root": {}, "admin": {}, "administrator": {}, "system": {}, "guest": {}, "客服": {}, "管理": {},
	"管理员": {}, "超级管理员": {}, "官方": {}, "站长": {}, "后台": {},
}

func reservedName(value string) bool {
	_, exists := reservedNames[strings.ToLower(value)]
	return exists
}

func validLabel(value string, min, max int) bool {
	return utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl) &&
		utf8.RuneCountInString(value) >= min && utf8.RuneCountInString(value) <= max
}

const (
	usernameMatch = `lower(username COLLATE "C") = lower($1::text COLLATE "C")`
	emailMatch    = `lower(email COLLATE "C") = $1::text COLLATE "C"`
	idMatch       = `id=$1`
)

type signupArgs struct {
	Username         string `json:"username"`
	Password         string `json:"password"`
	Nickname         string `json:"nickname"`
	Email            string `json:"email"`
	EmailCode        string `json:"email_code"`
	RegistrationCode string `json:"registration_code"`
}

type signinArgs struct {
	Username  string `json:"username"`
	Email     string `json:"email"`
	Password  string `json:"password"`
	EmailCode string `json:"email_code"`
}

type sessionResult struct {
	UserID                 int64     `json:"user_id"`
	Token                  string    `json:"token"`
	ExpiresAt              time.Time `json:"expires_at"`
	NeedGuide              bool      `json:"need_guide"`
	NeedsEmailVerification bool      `json:"needs_email_verification"`
}

func (s *Service) prepareSignup(ctx context.Context, _ rpc.Actor, ip string, raw json.RawMessage) (rpc.PreparedHandler, func(), error) {
	args, err := rpc.Decode[signupArgs](raw)
	if err != nil {
		return nil, nil, err
	}
	args.Username = strings.TrimSpace(args.Username)
	args.Nickname = strings.TrimSpace(args.Nickname)
	args.RegistrationCode = strings.TrimSpace(args.RegistrationCode)
	if !usernamePattern.MatchString(args.Username) || reservedName(args.Username) {
		return nil, nil, rpc.Invalid("Username must be 3 to 32 letters, digits or _@.- and must not be reserved")
	}
	if !validLabel(args.Nickname, 1, 16) || reservedName(args.Nickname) {
		return nil, nil, rpc.Invalid("Nickname must be 1 to 16 characters and must not be reserved")
	}
	if err := validateNewPassword(args.Password); err != nil {
		return nil, nil, err
	}
	if !s.authRates.signup(ip) {
		return nil, nil, authRateExceeded()
	}
	release, err := s.admitPasswordWork(ctx)
	if err != nil {
		return nil, nil, err
	}
	return func(ctx context.Context, call rpc.Call) (any, error) {
		return s.signup(ctx, call, args)
	}, release, nil
}

func (s *Service) signup(ctx context.Context, call rpc.Call, args signupArgs) (any, error) {
	var email *string
	var codeDigest []byte
	var err error
	switch {
	case args.RegistrationCode != "":
		if args.Email != "" || args.EmailCode != "" {
			return nil, rpc.Invalid("Choose a registration code or an email challenge")
		}
		codeDigest, err = registrationCodeDigest(args.RegistrationCode)
		if err != nil {
			return nil, err
		}
		// An inexpensive precheck avoids password hashing for unusable codes.
		var usable bool
		if err := call.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_registration_codes WHERE code_hash=$1 AND `+
			registrationUsable+`)`, codeDigest).Scan(&usable); err != nil {
			return nil, err
		}
		if !usable {
			return nil, invalidRegistrationCode()
		}
	case args.Email != "" || args.EmailCode != "":
		address, err := normalizeEmail(args.Email)
		if err != nil {
			return nil, err
		}
		if args.EmailCode == "" {
			return nil, verificationRequired()
		}
		verified, err := s.consumeChallenge(ctx, call.DB, challengeSignup, address, nil, args.EmailCode)
		if err != nil {
			return nil, err
		}
		if !verified {
			return rejectedChallenge(), nil
		}
		email = &address
	case s.cfg.RequireRegistrationEmail:
		return nil, verificationRequired()
	}

	hash, err := s.hashPassword(ctx, args.Password)
	if err != nil {
		return nil, err
	}
	var uid int64
	err = call.DB.QueryRow(ctx, `INSERT INTO accounts(username,nickname,password_hash,email,email_verified,registration_grant)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`, args.Username, args.Nickname, hash, email, email != nil, codeDigest != nil).Scan(&uid)
	if err != nil {
		return nil, checkAccountConflict(err)
	}
	if codeDigest != nil {
		if err := redeemRegistrationCode(ctx, call.DB, codeDigest, uid); err != nil {
			return nil, err
		}
	}
	result, err := s.issueSession(ctx, call.DB, uid, true, s.needsEmail(email != nil, codeDigest != nil))
	if err != nil {
		return nil, err
	}
	return result, emitAccount(call, "account.created", accountChange{UserID: uid})
}

func verificationRequired() error {
	return rpc.Error("verification_required", "Email verification is required", http.StatusForbidden)
}

func invalidCredentials() error {
	return rpc.Error("invalid_credentials", "The supplied credentials are invalid", http.StatusUnauthorized)
}

func (s *Service) prepareSignin(ctx context.Context, _ rpc.Actor, ip string, raw json.RawMessage) (rpc.PreparedHandler, func(), error) {
	args, err := rpc.Decode[signinArgs](raw)
	if err != nil {
		return nil, nil, err
	}
	username := strings.TrimSpace(args.Username)
	if len(username) > 255 || strings.ContainsRune(username, 0) || (username == "") == (args.Email == "") {
		return nil, nil, rpc.Invalid("Supply either a username or an email address")
	}
	byCode := args.EmailCode != ""
	if (args.Password != "") == byCode || (byCode && args.Email == "") {
		return nil, nil, rpc.Invalid("Supply a password or an email challenge code")
	}
	predicate, identity := usernameMatch, username
	if args.Email != "" {
		address, err := normalizeEmail(args.Email)
		if err != nil {
			return nil, nil, err
		}
		predicate, identity = emailMatch, address
	}
	if err := s.authRates.signin(ip, identity, args.Email != ""); err != nil {
		return nil, nil, err
	}
	var release func()
	if !byCode {
		release, err = s.admitPasswordWork(ctx)
		if err != nil {
			return nil, nil, err
		}
	}
	return func(ctx context.Context, call rpc.Call) (any, error) {
		return s.signin(ctx, call, args, predicate, identity)
	}, release, nil
}

func (s *Service) signin(ctx context.Context, call rpc.Call, args signinArgs, predicate, identity string) (any, error) {
	a, err := findAccount(ctx, call.DB, predicate, identity, true)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	found := err == nil
	if found && !s.authRates.signinAccount(a.id) {
		return nil, authRateExceeded()
	}
	var upgradedHash string
	if args.EmailCode != "" {
		if !found || !a.emailVerified || !a.active() {
			return rejectedChallenge(), nil
		}
		verified, err := s.consumeChallenge(ctx, call.DB, challengeSignin, identity, &a.id, args.EmailCode)
		if err != nil {
			return nil, err
		}
		if !verified {
			return rejectedChallenge(), nil
		}
	} else {
		password := passwordForCredential(a.passwordHash, args.Password)
		valid, upgrade, err := s.verifyPassword(ctx, a.passwordHash, password, a.username)
		if err != nil {
			return nil, err
		}
		if !found || !valid {
			return nil, invalidCredentials()
		}
		if err := a.signinError(); err != nil {
			return nil, err
		}
		if upgrade {
			upgradedHash, err = s.hashPassword(ctx, password)
			if err != nil {
				return nil, err
			}
		}
	}
	if err := finishSignin(ctx, call.DB, a, upgradedHash); err != nil {
		return nil, err
	}
	return s.issueSession(ctx, call.DB, a.id, a.firstLogin, s.needsEmail(a.emailVerified, a.registrationGrant))
}

func finishSignin(ctx context.Context, db rpc.DB, a accountState, upgradedHash string) error {
	if !a.firstLogin && a.status != "banned" && upgradedHash == "" {
		return nil
	}
	_, err := db.Exec(ctx, `UPDATE accounts SET is_first_login=false,
		password_hash=COALESCE(NULLIF($2,''),password_hash), status='active', ban_until=NULL, ban_reason='' WHERE id=$1`, a.id, upgradedHash)
	return err
}

const sessionTokenBytes = 32

func newToken() (string, error) {
	var entropy [sessionTokenBytes]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	defer clear(entropy[:])
	return base64.RawURLEncoding.EncodeToString(entropy[:]), nil
}

func validToken(token string) bool {
	if len(token) != base64.RawURLEncoding.EncodedLen(sessionTokenBytes) {
		return false
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(token)
	defer clear(decoded)
	return err == nil && len(decoded) == sessionTokenBytes
}

func tokenHash(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

func (s *Service) issueSession(ctx context.Context, db rpc.DB, uid int64, firstLogin, needsEmail bool) (sessionResult, error) {
	token, err := newToken()
	if err != nil {
		return sessionResult{}, err
	}
	result := sessionResult{UserID: uid, Token: token, NeedGuide: firstLogin, NeedsEmailVerification: needsEmail}
	err = db.QueryRow(ctx, `WITH activity AS (
		INSERT INTO user_activity(user_id,last_active) VALUES ($1,statement_timestamp())
		ON CONFLICT (user_id) DO UPDATE SET last_active=greatest(user_activity.last_active,excluded.last_active)
	) INSERT INTO account_sessions(user_id, token_hash, expires_at)
		VALUES ($1,$2,clock_timestamp()+$3::double precision * interval '1 second') RETURNING expires_at`,
		uid, tokenHash(token), s.cfg.SessionTTL.Seconds()).Scan(&result.ExpiresAt)
	return result, err
}

func (s *Service) Authenticate(ctx context.Context, token string) (rpc.Actor, error) {
	if !validToken(token) {
		return rpc.Actor{}, rpc.Unauthenticated()
	}
	var actor rpc.Actor
	var verified, grant bool
	hash := tokenHash(token)
	err := s.pool.QueryRow(ctx, `SELECT a.id,a.site_admin,a.email_verified,a.registration_grant FROM account_sessions s
		JOIN accounts a ON a.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>clock_timestamp()
		AND (a.status='active' OR (a.status='banned' AND a.ban_until<=clock_timestamp()))`, hash).
		Scan(&actor.ID, &actor.Admin, &verified, &grant)
	if errors.Is(err, pgx.ErrNoRows) {
		return rpc.Actor{}, rpc.Unauthenticated()
	}
	if err != nil {
		return rpc.Actor{}, err
	}
	actor.Session = hash
	actor.Chat = !s.needsEmail(verified, grant)
	return actor, nil
}

type sessionView struct {
	ID        string    `json:"session_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Current   bool      `json:"current"`
}

func (s *Service) listSessions(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	if err := decodeEmpty(raw); err != nil {
		return nil, err
	}
	if _, err := currentAccount(ctx, call, false); err != nil {
		return nil, err
	}
	rows, err := call.DB.Query(ctx, `SELECT id::text,created_at,expires_at,token_hash=$2 FROM account_sessions
		WHERE user_id=$1 AND expires_at>clock_timestamp() ORDER BY created_at DESC,id DESC LIMIT 100`, call.Actor.ID, call.Actor.Session)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := make([]sessionView, 0)
	for rows.Next() {
		var session sessionView
		if err := rows.Scan(&session.ID, &session.CreatedAt, &session.ExpiresAt, &session.Current); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return struct {
		Sessions []sessionView `json:"sessions"`
	}{sessions}, rows.Err()
}

type revokeArgs struct {
	SessionID string `json:"session_id"`
	All       bool   `json:"all"`
}

type revokeResult struct {
	Revoked int64 `json:"revoked"`
}

func (s *Service) revokeSessions(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	args, err := rpc.Decode[revokeArgs](raw)
	if err != nil {
		return nil, err
	}
	if (args.SessionID != "") == args.All {
		return nil, rpc.Invalid("Supply a session ID or request revocation of all sessions")
	}
	var sessionID pgtype.UUID
	if !args.All && (len(args.SessionID) != 36 || sessionID.Scan(args.SessionID) != nil) {
		return nil, rpc.Invalid("Invalid session ID")
	}
	if _, err := currentAccount(ctx, call, true); err != nil {
		return nil, err
	}
	tag, err := call.DB.Exec(ctx, `DELETE FROM account_sessions WHERE user_id=$1 AND ($2::uuid IS NULL OR id=$2)`, call.Actor.ID, sessionID)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() > 0 {
		if err := emitAccount(call, "account.sessions_changed", accountChange{UserID: call.Actor.ID}); err != nil {
			return nil, err
		}
	}
	return revokeResult{Revoked: tag.RowsAffected()}, nil
}

func (s *Service) logout(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	if err := decodeEmpty(raw); err != nil {
		return nil, err
	}
	if _, err := currentAccount(ctx, call, true); err != nil {
		return nil, err
	}
	tag, err := call.DB.Exec(ctx, `DELETE FROM account_sessions WHERE user_id=$1 AND token_hash=$2`, call.Actor.ID, call.Actor.Session)
	if err != nil {
		return nil, err
	}
	return revokeResult{Revoked: tag.RowsAffected()}, emitAccount(call, "account.sessions_changed", accountChange{UserID: call.Actor.ID})
}

// Package account implements CsAC identity, credentials and account administration.
package account

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"csac/v2/internal/rpc"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultSessionTTL = 30 * 24 * time.Hour
	coolingPeriodDays = 14
	coolingPeriod     = coolingPeriodDays * 24 * time.Hour
	defaultPatAction  = "拍了拍"
)

// Config controls independent signup and chat membership policies.
type Config struct {
	RequireRegistrationEmail bool
	RequireMemberEmail       bool
	SessionTTL               time.Duration
	Deliver                  func(ctx context.Context, recipient, subject, body string) error
	// RegistrationCodeKey is the optional 32-byte decoded deployment key.
	// A stable key also keeps outstanding email challenges valid across instances.
	RegistrationCodeKey []byte
	// PasswordPepper is a separate 32-byte decoded deployment secret. New
	// copies the value and panics if it is nonempty and the length is invalid.
	PasswordPepper []byte
}

type Service struct {
	pool           *pgxpool.Pool
	cfg            Config
	hashSlots      chan struct{}
	authRates      authRateLimits
	passwordPepper *[passwordPepperBytes]byte
	codeCipher     cipher.AEAD
	challengeKey   [32]byte
}

var _ rpc.Authenticator = (*Service)(nil)

func New(pool *pgxpool.Pool, cfg Config) *Service {
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = defaultSessionTTL
	}
	if n := len(cfg.PasswordPepper); n != 0 && n != passwordPepperBytes {
		panic("account: PasswordPepper must be empty or exactly 32 bytes")
	}
	workers := passwordWorkers
	if pool != nil {
		// Leave one connection for other work whenever the pool has room.
		workers = min(workers, max(1, int(pool.Config().MaxConns)-1))
	}
	s := &Service{pool: pool, cfg: cfg, hashSlots: make(chan struct{}, workers)}
	s.cfg.PasswordPepper = nil
	if len(cfg.PasswordPepper) > 0 {
		s.passwordPepper = new([passwordPepperBytes]byte)
		copy(s.passwordPepper[:], cfg.PasswordPepper)
	}
	s.cfg.RegistrationCodeKey = nil
	if len(cfg.RegistrationCodeKey) == 32 {
		// These constructors accept a 32-byte key and the standard AES block size.
		block, err := aes.NewCipher(cfg.RegistrationCodeKey)
		if err != nil {
			panic(err)
		}
		s.codeCipher, err = cipher.NewGCMWithRandomNonce(block)
		if err != nil {
			panic(err)
		}
		s.challengeKey = macSum(cfg.RegistrationCodeKey, []byte("csac/v2 account email challenge key"))
	} else if _, err := rand.Read(s.challengeKey[:]); err != nil {
		panic(err)
	}
	return s
}

func (s *Service) Register(reg *rpc.Registry) {
	reg.Register("account.signup.challenge", rpc.Method{Access: rpc.Public, Write: true, Handle: s.sendChallenge(challengeSignup)})
	reg.Register("account.signup", rpc.Method{Access: rpc.Public, Write: true, Prepare: s.prepareSignup})
	reg.Register("account.signin.challenge", rpc.Method{Access: rpc.Public, Write: true, Handle: s.sendChallenge(challengeSignin)})
	reg.Register("account.signin", rpc.Method{Access: rpc.Public, Write: true, Prepare: s.prepareSignin})
	reg.Register("account.logout", rpc.Method{Access: rpc.SignedIn, Write: true, Handle: s.logout})
	reg.Register("account.session.list", rpc.Method{Access: rpc.SignedIn, Handle: s.listSessions})
	reg.Register("account.session.revoke", rpc.Method{Access: rpc.SignedIn, Write: true, Handle: s.revokeSessions})
	reg.Register("account.email.challenge", rpc.Method{Access: rpc.SignedIn, Write: true, Handle: s.sendChallenge(challengeBind)})
	reg.Register("account.email.bind", rpc.Method{Access: rpc.SignedIn, Write: true, Handle: s.bindEmail})
	reg.Register("account.self", rpc.Method{Access: rpc.SignedIn, Handle: s.self})
	reg.Register("account.lookup", rpc.Method{Access: rpc.Member, Handle: s.lookup})
	reg.Register("account.profile.update", rpc.Method{Access: rpc.SignedIn, Write: true, Dedup: true, Handle: s.updateProfile})
	reg.Register("account.password.change", rpc.Method{Access: rpc.SignedIn, Write: true, Prepare: s.preparePasswordChange})
	reg.Register("account.delete", rpc.Method{Access: rpc.SignedIn, Write: true, Handle: s.deleteAccount})
	reg.Register("account.restore.challenge", rpc.Method{Access: rpc.Public, Write: true, Handle: s.sendChallenge(challengeRestore)})
	reg.Register("account.restore", rpc.Method{Access: rpc.Public, Write: true, Handle: s.restoreByEmail})
	reg.Register("admin.account.ban", rpc.Method{Access: rpc.Admin, Write: true, Dedup: true, Handle: s.ban})
	reg.Register("admin.account.unban", rpc.Method{Access: rpc.Admin, Write: true, Dedup: true, Handle: s.unban})
	reg.Register("admin.account.restore", rpc.Method{Access: rpc.Admin, Write: true, Dedup: true, Handle: s.adminRestore})
	reg.Register("admin.registration_code.mint", rpc.Method{Access: rpc.Admin, Write: true, Handle: s.mintRegistrationCode})
	reg.Register("admin.registration_code.list", rpc.Method{Access: rpc.Admin, Handle: s.listRegistrationCodes})
	reg.Register("admin.registration_code.detail", rpc.Method{Access: rpc.Admin, Handle: s.registrationCodeDetail})
	reg.Register("admin.registration_code.revoke", rpc.Method{Access: rpc.Admin, Write: true, Dedup: true, Handle: s.revokeRegistrationCode})
}

func (s *Service) needsEmail(verified, grant bool) bool {
	return s.cfg.RequireMemberEmail && !verified && !grant
}

// Maintenance expires timed bans and credentials, then purges a bounded batch
// of accounts past the recovery window. The deadlines also apply on reads.
func (s *Service) Maintenance(ctx context.Context) error {
	err := s.expireCredentials(ctx)
	return errors.Join(err, s.purgeExpiredAccounts(ctx))
}

func (s *Service) expireCredentials(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Each update is bounded. Skip busy account rows; a later pass can claim them.
		if _, err := tx.Exec(ctx, `WITH expired AS (
			SELECT id FROM accounts WHERE status='banned' AND ban_until <= clock_timestamp()
			ORDER BY ban_until, id LIMIT 500 FOR NO KEY UPDATE SKIP LOCKED
		) UPDATE accounts a SET status='active', ban_until=NULL, ban_reason='' FROM expired WHERE a.id=expired.id`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM account_sessions WHERE id IN (
			SELECT id FROM account_sessions WHERE expires_at <= clock_timestamp() ORDER BY expires_at LIMIT 1000
			FOR UPDATE SKIP LOCKED)`); err != nil {
			return err
		}
		// Preserve the send-rate window even after the challenge has expired.
		_, err := tx.Exec(ctx, `DELETE FROM account_email_challenges WHERE id IN (
			SELECT id FROM account_email_challenges WHERE expires_at < clock_timestamp() - interval '1 hour'
			ORDER BY expires_at LIMIT 1000 FOR UPDATE SKIP LOCKED)`)
		return err
	})
}

type accountState struct {
	id                                 int64
	username, passwordHash, status     string
	banReason                          string
	banUntil                           *time.Time
	email                              *string
	emailVerified, registrationGrant   bool
	siteAdmin, firstLogin, recoverable bool
	banExpired                         bool
}

// predicate is always a package SQL literal. User values are bound separately.
func findAccount(ctx context.Context, db rpc.DB, predicate string, value any, lock bool) (accountState, error) {
	query := `SELECT id, username, password_hash, status, email, email_verified, registration_grant, site_admin, is_first_login,
		ban_until,ban_reason,COALESCE(ban_until <= clock_timestamp(), false),
		COALESCE(status='deleted' AND purged_at IS NULL AND deleted_at > clock_timestamp() - $2::double precision * interval '1 second', false)
		FROM accounts WHERE ` + predicate
	if lock {
		query += ` FOR NO KEY UPDATE`
	}
	var a accountState
	err := db.QueryRow(ctx, query, value, coolingPeriod.Seconds()).Scan(&a.id, &a.username, &a.passwordHash, &a.status,
		&a.email, &a.emailVerified, &a.registrationGrant, &a.siteAdmin, &a.firstLogin, &a.banUntil, &a.banReason, &a.banExpired, &a.recoverable)
	return a, err
}

func (a accountState) active() bool {
	return a.status == "active" || (a.status == "banned" && a.banExpired)
}

func (a accountState) signinError() error {
	if a.active() {
		return nil
	}
	if a.status == "deleted" {
		if a.recoverable {
			return rpc.Error("account_deleted", "Account deleted. It can be restored during the cooling-off period", http.StatusForbidden)
		}
		return rpc.Error("account_deleted", "Account permanently deleted", http.StatusForbidden)
	}
	message := "Account banned"
	if a.banUntil != nil {
		message += " until " + a.banUntil.UTC().Format(time.RFC3339)
	}
	if a.banReason != "" {
		message += ". Reason: " + a.banReason
	}
	return rpc.Error("account_banned", message, http.StatusForbidden)
}

func currentAccount(ctx context.Context, call rpc.Call, lock bool) (accountState, error) {
	if call.Actor.ID <= 0 || len(call.Actor.Session) != 32 {
		return accountState{}, rpc.Unauthenticated()
	}
	var a accountState
	var err error
	if lock {
		// Take the account lock before checking the session in a fresh statement.
		// This serializes changes with password rotation, bans and deletion.
		a, err = findAccount(ctx, call.DB, `id=$1`, call.Actor.ID, true)
		if err == nil && a.active() {
			var valid bool
			err = call.DB.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_sessions WHERE user_id=$1 AND token_hash=$2 AND expires_at>clock_timestamp())`,
				a.id, call.Actor.Session).Scan(&valid)
			if err == nil && !valid {
				return accountState{}, rpc.Unauthenticated()
			}
		}
	} else {
		// Read the account and session together for sensitive reads.
		a, err = findAccount(ctx, call.DB, `id=(SELECT user_id FROM account_sessions WHERE token_hash=$1 AND expires_at>clock_timestamp())`, call.Actor.Session, false)
	}
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (a.id != call.Actor.ID || !a.active())) {
		return accountState{}, rpc.Unauthenticated()
	}
	return a, err
}

func requireAdmin(ctx context.Context, call rpc.Call, lock bool) error {
	a, err := currentAccount(ctx, call, lock)
	if err != nil {
		return err
	}
	if !a.siteAdmin {
		return rpc.Denied("Administrator access required")
	}
	return nil
}

type accountChange struct {
	UserID int64  `json:"user_id"`
	Status string `json:"status,omitempty"`
}

func emitAccount(call rpc.Call, kind string, change accountChange) error {
	return call.Emit(kind, []int64{change.UserID}, change)
}

func checkAccountConflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case "accounts_username_folded":
			return rpc.Conflict("Username already registered")
		case "accounts_email_folded":
			return rpc.Conflict("Email already registered")
		}
	}
	return err
}

func decodeEmpty(raw json.RawMessage) error {
	_, err := rpc.Decode[struct{}](raw)
	return err
}

package account

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/mail"
	"net/netip"
	"strings"
	"time"

	"csac/v2/internal/rpc"

	"github.com/jackc/pgx/v5"
)

type challengePurpose string

const (
	challengeSignup  challengePurpose = "signup"
	challengeSignin  challengePurpose = "signin"
	challengeBind    challengePurpose = "bind"
	challengeRestore challengePurpose = "restore"

	codeTTL        = 10 * time.Minute
	restoreTTL     = time.Hour
	resendAfter    = time.Minute
	maxHourlySends = 5
	maxGuesses     = 5
)

func normalizeEmail(input string) (string, error) {
	address := strings.ToLower(strings.TrimSpace(input))
	if address == "" || len(address) > 255 || strings.ContainsAny(address, "\r\n\x00") {
		return "", rpc.Invalid("Invalid email address")
	}
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address || parsed.Name != "" {
		return "", rpc.Invalid("Invalid email address")
	}
	return address, nil
}

func normalizedOrigin(input string) string {
	address, err := netip.ParseAddr(input)
	if err != nil {
		addressPort, err := netip.ParseAddrPort(input)
		if err != nil {
			return "unknown"
		}
		address = addressPort.Addr()
	}
	return address.WithZone("").Unmap().String()
}

func macSum(key []byte, parts ...[]byte) [32]byte {
	mac := hmac.New(sha256.New, key)
	for _, part := range parts {
		_, _ = mac.Write(part)
	}
	var digest [32]byte
	mac.Sum(digest[:0])
	return digest
}

func (s *Service) emailKey(purpose challengePurpose, address string) [32]byte {
	return macSum(s.challengeKey[:], []byte("email\x00"+string(purpose)+"\x00"), []byte(address))
}

func (s *Service) challengeMAC(purpose challengePurpose, emailKey, nonce []byte, secret string) [32]byte {
	return macSum(s.challengeKey[:], []byte("proof\x00"+string(purpose)+"\x00"), emailKey, nonce, []byte(secret))
}

func challengeSecret(purpose challengePurpose) (string, time.Duration, error) {
	if purpose == challengeRestore {
		secret, err := newToken()
		return secret, restoreTTL, err
	}
	n, err := rand.Int(rand.Reader, big.NewInt(900_000))
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%06d", n.Int64()+100_000), codeTTL, nil
}

type emailArgs struct {
	Email string `json:"email"`
}

type challengeReceipt struct {
	ExpiresIn   int64 `json:"expires_in"`
	ResendAfter int64 `json:"resend_after"`
}

func (s *Service) sendChallenge(purpose challengePurpose) rpc.Handler {
	return func(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
		args, err := rpc.Decode[emailArgs](raw)
		if err != nil {
			return nil, err
		}
		address, err := normalizeEmail(args.Email)
		if err != nil {
			return nil, err
		}
		if s.cfg.Deliver == nil {
			return nil, rpc.Error("mail_unavailable", "Email delivery is unavailable", http.StatusServiceUnavailable)
		}
		subject, eligible, err := challengeRecipient(ctx, call, purpose, address)
		if err != nil {
			return nil, err
		}
		emailKey := s.emailKey(purpose, address)
		originKey := macSum(s.challengeKey[:], []byte("origin\x00"+string(purpose)+"\x00"), []byte(normalizedOrigin(call.IP)))
		if err := lockMailbox(ctx, call.DB, emailKey); err != nil {
			return nil, err
		}
		if _, err := call.DB.Exec(ctx, `SELECT pg_advisory_xact_lock(2010063521, $1::int)`, advisoryKey(originKey)); err != nil {
			return nil, err
		}
		if err := checkSendLimits(ctx, call.DB, purpose, emailKey, originKey); err != nil {
			return nil, err
		}
		secret, ttl, err := challengeSecret(purpose)
		if err != nil {
			return nil, err
		}
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		proof := s.challengeMAC(purpose, emailKey[:], nonce[:], secret)
		// Ineligible addresses share the same response and rate limit. Record an
		// already used challenge to avoid holding a redeemable secret for them.
		_, err = call.DB.Exec(ctx, `INSERT INTO account_email_challenges(purpose,subject_id,email_key,origin_key,secret_hash,nonce,expires_at,used_at)
			VALUES ($1,$2,$3,$4,$5,$6,clock_timestamp()+$7::double precision * interval '1 second',
			CASE WHEN $8::boolean THEN NULL ELSE clock_timestamp() END)`, purpose, subject, emailKey[:], originKey[:], proof[:], nonce[:], ttl.Seconds(), eligible)
		if err != nil {
			return nil, err
		}
		if eligible {
			if err := s.deliverChallenge(ctx, purpose, address, secret, ttl); err != nil {
				return nil, err
			}
		}
		return challengeReceipt{int64(ttl.Seconds()), int64(resendAfter.Seconds())}, nil
	}
}

func challengeRecipient(ctx context.Context, call rpc.Call, purpose challengePurpose, address string) (*int64, bool, error) {
	switch purpose {
	case challengeBind:
		a, err := currentAccount(ctx, call, true)
		if err != nil {
			return nil, false, err
		}
		if a.emailVerified && a.email != nil {
			return nil, false, rpc.Conflict("A verified mailbox is already bound")
		}
		var available bool
		err = call.DB.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM accounts WHERE `+emailMatch+` AND id<>$2)`, address, a.id).Scan(&available)
		return &a.id, available, err
	case challengeSignin, challengeRestore:
		a, err := findAccount(ctx, call.DB, emailMatch, address, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		eligible := a.emailVerified && ((purpose == challengeSignin && a.active()) || (purpose == challengeRestore && a.recoverable))
		return &a.id, eligible, nil
	case challengeSignup:
		var available bool
		err := call.DB.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM accounts WHERE `+emailMatch+`)`, address).Scan(&available)
		return nil, available, err
	default:
		return nil, false, errors.New("unexpected email challenge purpose")
	}
}

func advisoryKey(key [32]byte) int32 {
	return int32(binary.BigEndian.Uint32(key[:4]))
}

func lockMailbox(ctx context.Context, db rpc.DB, emailKey [32]byte) error {
	_, err := db.Exec(ctx, `SELECT pg_advisory_xact_lock(2010063520, $1::int)`, advisoryKey(emailKey))
	return err
}

func checkSendLimits(ctx context.Context, db rpc.DB, purpose challengePurpose, emailKey, originKey [32]byte) error {
	// Both locks are already held. Check the independently indexed windows so
	// different addresses on one origin and distributed sends both count.
	const window = ` AND created_at > clock_timestamp()-interval '1 hour'`
	query := `SELECT count(*), COALESCE(max(created_at)>clock_timestamp()-$3::double precision * interval '1 second',false)
		FROM account_email_challenges WHERE purpose=$1 AND `
	for _, limit := range []struct {
		where string
		key   [32]byte
	}{{`email_key=$2`, emailKey}, {`origin_key=$2`, originKey}} {
		var count int64
		var withinCooldown bool
		if err := db.QueryRow(ctx, query+limit.where+window, purpose, limit.key[:], resendAfter.Seconds()).Scan(&count, &withinCooldown); err != nil {
			return err
		}
		if withinCooldown || count >= maxHourlySends {
			return rpc.RateLimited("Too many email requests. Try again later")
		}
	}
	return nil
}

func (s *Service) deliverChallenge(ctx context.Context, purpose challengePurpose, address, secret string, ttl time.Duration) error {
	subject := "CsAC verification code"
	if purpose == challengeRestore {
		subject = "CsAC account recovery"
	}
	body := fmt.Sprintf("%s\n\nEnter this code in CsAC within %d minutes.\n", secret, ttl/time.Minute)
	deliveryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.cfg.Deliver(deliveryCtx, address, subject, body); err != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Delivery errors can contain the destination, message, or provider credentials.
		return rpc.Error("mail_unavailable", "Email delivery failed. Try again later", http.StatusBadGateway)
	}
	return ctx.Err()
}

type challengeRejection struct {
	Verified bool   `json:"verified"`
	Reason   string `json:"error"`
}

// A bad guess is a business result so the registry commits the attempt count.
// An RPC fault would roll back the security update along with the command.
func rejectedChallenge() challengeRejection {
	return challengeRejection{Verified: false, Reason: "invalid_email_challenge"}
}

func validChallengeSecret(purpose challengePurpose, secret string) bool {
	if purpose == challengeRestore {
		return validToken(secret)
	}
	if len(secret) != 6 {
		return false
	}
	for i := range len(secret) {
		if secret[i] < '0' || secret[i] > '9' {
			return false
		}
	}
	return true
}

func (s *Service) consumeChallenge(ctx context.Context, db rpc.DB, purpose challengePurpose, address string, subjectID *int64, secret string) (bool, error) {
	if !validChallengeSecret(purpose, secret) {
		return false, nil
	}
	emailKey := s.emailKey(purpose, address)
	if err := lockMailbox(ctx, db, emailKey); err != nil {
		return false, err
	}
	var id int64
	var nonce, storedHash []byte
	var usable bool
	err := db.QueryRow(ctx, `SELECT id,nonce,secret_hash,used_at IS NULL AND attempts<$4 AND expires_at>clock_timestamp()
		FROM account_email_challenges WHERE purpose=$1 AND email_key=$2 AND subject_id IS NOT DISTINCT FROM $3::bigint
		ORDER BY id DESC LIMIT 1 FOR UPDATE`, purpose, emailKey[:], subjectID, maxGuesses).Scan(&id, &nonce, &storedHash, &usable)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !usable) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	proof := s.challengeMAC(purpose, emailKey[:], nonce, secret)
	if !hmac.Equal(storedHash, proof[:]) {
		_, err := db.Exec(ctx, `UPDATE account_email_challenges SET attempts=attempts+1 WHERE id=$1`, id)
		return false, err
	}
	tag, err := db.Exec(ctx, `UPDATE account_email_challenges SET used_at=clock_timestamp() WHERE id=$1 AND expires_at>clock_timestamp()`, id)
	return tag.RowsAffected() == 1, err
}

func invalidateChallenges(ctx context.Context, db rpc.DB, uid int64) error {
	_, err := db.Exec(ctx, `UPDATE account_email_challenges SET used_at=clock_timestamp() WHERE subject_id=$1 AND used_at IS NULL`, uid)
	return err
}

type bindArgs struct {
	Email     string `json:"email"`
	EmailCode string `json:"email_code"`
}

type verifiedEmailResult struct {
	EmailVerified bool `json:"email_verified"`
}

func (s *Service) bindEmail(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	args, err := rpc.Decode[bindArgs](raw)
	if err != nil {
		return nil, err
	}
	address, err := normalizeEmail(args.Email)
	if err != nil {
		return nil, err
	}
	a, err := currentAccount(ctx, call, true)
	if err != nil {
		return nil, err
	}
	if a.emailVerified && a.email != nil {
		if strings.EqualFold(*a.email, address) {
			return verifiedEmailResult{true}, nil
		}
		return nil, rpc.Conflict("A verified mailbox is already bound")
	}
	verified, err := s.consumeChallenge(ctx, call.DB, challengeBind, address, &a.id, args.EmailCode)
	if err != nil {
		return nil, err
	}
	if !verified {
		return rejectedChallenge(), nil
	}
	if _, err := call.DB.Exec(ctx, `UPDATE accounts SET email=$2,email_verified=true WHERE id=$1`, a.id, address); err != nil {
		return nil, checkAccountConflict(err)
	}
	return verifiedEmailResult{true}, emitAccount(call, "account.email_verified", accountChange{UserID: a.id})
}

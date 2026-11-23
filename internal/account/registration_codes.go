package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"csac/v2/internal/rpc"

	"github.com/jackc/pgx/v5"
)

const registrationCodePrefix = "CSAC-"
const registrationUsable = `revoked_at IS NULL AND used_count < max_uses AND (expires_at IS NULL OR expires_at > clock_timestamp())`

func invalidRegistrationCode() error {
	return rpc.Error("invalid_registration_code", "Registration code is invalid, expired, exhausted or revoked", http.StatusBadRequest)
}

func registrationCodeDigest(code string) ([]byte, error) {
	suffix, ok := strings.CutPrefix(code, registrationCodePrefix)
	if !ok || len(suffix) != 32 {
		return nil, invalidRegistrationCode()
	}
	for _, c := range suffix {
		if !(c >= 'A' && c <= 'Z' || c >= '2' && c <= '7') {
			return nil, invalidRegistrationCode()
		}
	}
	return tokenHash(code), nil
}

func redeemRegistrationCode(ctx context.Context, db rpc.DB, digest []byte, uid int64) error {
	var codeID int64
	err := db.QueryRow(ctx, `UPDATE account_registration_codes SET used_count=used_count+1
		WHERE code_hash=$1 AND `+registrationUsable+` RETURNING id`, digest).Scan(&codeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return invalidRegistrationCode()
	}
	if err != nil {
		return err
	}
	_, err = db.Exec(ctx, `INSERT INTO account_registration_redemptions(user_id,code_id) VALUES ($1,$2)`, uid, codeID)
	return err
}

type codeInfo struct {
	ID            int64  `json:"id"`
	Status        string `json:"status"`
	MaxUses       int32  `json:"max_uses"`
	UsedCount     int32  `json:"used_count"`
	RemainingUses int32  `json:"remaining_uses"`
	CreatedAt     int64  `json:"created_at"`
	CreatedBy     int64  `json:"created_by"`
	ExpiresAt     *int64 `json:"expires_at"`
	RevokedAt     *int64 `json:"revoked_at"`
	RevokedBy     *int64 `json:"revoked_by"`
}

type codeSecret struct {
	codeInfo
	Code string `json:"code"`
}

const codeColumns = `id,max_uses,used_count,created_at,created_by,expires_at,revoked_at,revoked_by,clock_timestamp()`

func readCodeInfo(row pgx.Row, extra ...any) (codeInfo, error) {
	var info codeInfo
	var created, now time.Time
	var expires, revoked *time.Time
	dest := []any{&info.ID, &info.MaxUses, &info.UsedCount, &created, &info.CreatedBy, &expires, &revoked, &info.RevokedBy, &now}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return codeInfo{}, err
	}
	info.CreatedAt = created.Unix()
	if expires != nil {
		info.ExpiresAt = new(expires.Unix())
	}
	if revoked != nil {
		info.RevokedAt = new(revoked.Unix())
	}
	info.RemainingUses = info.MaxUses - info.UsedCount
	switch {
	case revoked != nil:
		info.Status = "revoked"
	case expires != nil && !expires.After(now):
		info.Status = "expired"
	case info.RemainingUses == 0:
		info.Status = "exhausted"
	default:
		info.Status = "active"
	}
	return info, nil
}

type mintArgs struct {
	MaxUses      int32  `json:"max_uses"`
	ExpiresAt    *int64 `json:"expires_at"`
	NeverExpires bool   `json:"never_expires"`
}

func codeKeyUnavailable() error {
	return rpc.Error("registration_code_unavailable", "Registration code encryption key is unavailable", http.StatusServiceUnavailable)
}

func (s *Service) mintRegistrationCode(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	args, err := rpc.Decode[mintArgs](raw)
	if err != nil {
		return nil, err
	}
	if args.MaxUses <= 0 || (args.ExpiresAt != nil) == args.NeverExpires {
		return nil, rpc.Invalid("Supply max_uses and either expires_at or never_expires")
	}
	var expires *time.Time
	if args.ExpiresAt != nil {
		if *args.ExpiresAt <= 0 || *args.ExpiresAt > 253402300799 {
			return nil, rpc.Invalid("Invalid expiration timestamp")
		}
		expires = new(time.Unix(*args.ExpiresAt, 0).UTC())
	}
	if err := requireAdmin(ctx, call, true); err != nil {
		return nil, err
	}
	if s.codeCipher == nil {
		return nil, codeKeyUnavailable()
	}
	var random [20]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	code := registrationCodePrefix + base32.StdEncoding.EncodeToString(random[:])
	digest := sha256.Sum256([]byte(code))
	sealed := s.codeCipher.Seal(nil, nil, []byte(code), digest[:])
	info, err := readCodeInfo(call.DB.QueryRow(ctx, `INSERT INTO account_registration_codes(code_hash,code_ciphertext,max_uses,created_by,expires_at)
		SELECT $1,$2,$3,$4,$5::timestamptz WHERE $5 IS NULL OR $5>clock_timestamp() RETURNING `+codeColumns,
		digest[:], sealed, args.MaxUses, call.Actor.ID, expires))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, rpc.Invalid("Expiration must be in the future")
	}
	if err != nil {
		return nil, err
	}
	return codeSecret{codeInfo: info, Code: code}, nil
}

func codePage(limit int, before int64) (int, error) {
	if limit < 0 || limit > 100 || before < 0 {
		return 0, rpc.Invalid("Limit must be 1 to 100 and cursor must be a positive ID")
	}
	if limit == 0 {
		return 30, nil
	}
	return limit, nil
}

type codeListArgs struct {
	Limit    int   `json:"limit"`
	BeforeID int64 `json:"before_id"`
}

type codeList struct {
	Codes        []codeInfo `json:"codes"`
	NextBeforeID *int64     `json:"next_before_id"`
}

func (s *Service) listRegistrationCodes(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	args, err := rpc.Decode[codeListArgs](raw)
	if err != nil {
		return nil, err
	}
	limit, err := codePage(args.Limit, args.BeforeID)
	if err != nil {
		return nil, err
	}
	if err := requireAdmin(ctx, call, false); err != nil {
		return nil, err
	}
	rows, err := call.DB.Query(ctx, `SELECT `+codeColumns+` FROM account_registration_codes
		WHERE ($1::bigint=0 OR id<$1) ORDER BY id DESC LIMIT $2`, args.BeforeID, limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := codeList{Codes: make([]codeInfo, 0)}
	for rows.Next() {
		info, err := readCodeInfo(rows)
		if err != nil {
			return nil, err
		}
		result.Codes = append(result.Codes, info)
	}
	if len(result.Codes) > limit {
		result.Codes = result.Codes[:limit]
		result.NextBeforeID = &result.Codes[limit-1].ID
	}
	return result, rows.Err()
}

type detailArgs struct {
	ID           int64 `json:"id"`
	Limit        int   `json:"limit"`
	BeforeUserID int64 `json:"before_user_id"`
}

type codeRedemption struct {
	UserID       int64  `json:"user_id"`
	Username     string `json:"username"`
	Nickname     string `json:"nickname"`
	RegisteredAt int64  `json:"registered_at"`
}

type codeDetail struct {
	RegistrationCode codeSecret       `json:"registration_code"`
	Redemptions      []codeRedemption `json:"redemptions"`
	NextBeforeUserID *int64           `json:"next_before_user_id"`
}

func (s *Service) registrationCodeDetail(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	args, err := rpc.Decode[detailArgs](raw)
	if err != nil {
		return nil, err
	}
	if args.ID <= 0 {
		return nil, rpc.Invalid("Supply a valid registration code ID")
	}
	limit, err := codePage(args.Limit, args.BeforeUserID)
	if err != nil {
		return nil, err
	}
	result := codeDetail{Redemptions: make([]codeRedemption, 0)}
	err = pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		call.DB = tx
		if err := requireAdmin(ctx, call, false); err != nil {
			return err
		}
		if s.codeCipher == nil {
			return codeKeyUnavailable()
		}
		var digest, sealed []byte
		info, err := readCodeInfo(tx.QueryRow(ctx, `SELECT `+codeColumns+`,code_hash,code_ciphertext FROM account_registration_codes WHERE id=$1`, args.ID), &digest, &sealed)
		if err != nil {
			return err
		}
		if len(sealed) < s.codeCipher.Overhead() {
			return errors.New("invalid encrypted registration code")
		}
		plain, err := s.codeCipher.Open(nil, nil, sealed, digest)
		if err != nil {
			return fmt.Errorf("decrypt registration code: %w", err)
		}
		defer clear(plain)
		result.RegistrationCode = codeSecret{codeInfo: info, Code: string(plain)}
		rows, err := tx.Query(ctx, `SELECT r.user_id,a.username,a.nickname,r.registered_at FROM account_registration_redemptions r
			JOIN accounts a ON a.id=r.user_id WHERE r.code_id=$1 AND ($2::bigint=0 OR r.user_id<$2) ORDER BY r.user_id DESC LIMIT $3`,
			args.ID, args.BeforeUserID, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var redemption codeRedemption
			var registeredAt time.Time
			if err := rows.Scan(&redemption.UserID, &redemption.Username, &redemption.Nickname, &registeredAt); err != nil {
				return err
			}
			redemption.RegisteredAt = registeredAt.Unix()
			result.Redemptions = append(result.Redemptions, redemption)
		}
		if len(result.Redemptions) > limit {
			result.Redemptions = result.Redemptions[:limit]
			result.NextBeforeUserID = &result.Redemptions[limit-1].UserID
		}
		return rows.Err()
	})
	return result, err
}

type codeIDArgs struct {
	ID int64 `json:"id"`
}

func (s *Service) revokeRegistrationCode(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	args, err := rpc.Decode[codeIDArgs](raw)
	if err != nil {
		return nil, err
	}
	if args.ID <= 0 {
		return nil, rpc.Invalid("Supply a valid registration code ID")
	}
	if err := requireAdmin(ctx, call, true); err != nil {
		return nil, err
	}
	return readCodeInfo(call.DB.QueryRow(ctx, `UPDATE account_registration_codes SET revoked_at=COALESCE(revoked_at,clock_timestamp()),
		revoked_by=COALESCE(revoked_by,$2) WHERE id=$1 RETURNING `+codeColumns, args.ID, call.Actor.ID))
}

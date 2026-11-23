package account

import (
	"context"
	"crypto/md5" // Imported v1 credentials only.
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"csac/v2/internal/rpc"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

const (
	// RFC 9106's second recommended profile, sized for a shared server.
	argonMemory         = 64 * 1024 // KiB per derivation, across all lanes
	argonIterations     = 3
	argonLanes          = 4
	passwordSaltBytes   = 16
	passwordKeyBytes    = 32
	passwordPepperBytes = 32
	passwordWorkers     = 2
	maxLegacyCost       = 14
	minPasswordRunes    = 8

	argonPrefix         = "$argon2id$"
	pepperedArgonPrefix = "$csac$argon2id-hmac-sha256$1$"
	pepperDomain        = "csac/v2/password-verifier\x00"
)

// New passwords count Unicode code points. Verification uses the exact bytes
// of existing credentials and does not apply this creation policy.
func validateNewPassword(password string) error {
	if !utf8.ValidString(password) || utf8.RuneCountInString(password) < minPasswordRunes {
		return rpc.Invalid(fmt.Sprintf("Password must contain at least %d Unicode characters", minPasswordRunes))
	}
	return nil
}

// Admit one password command before any queries or row locks. A single slot
// covers verification and any rehash, so an open transaction never waits for
// capacity between derivations.
func (s *Service) admitPasswordWork(ctx context.Context) (release func(), err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.hashSlots <- struct{}{}:
		return func() { <-s.hashSlots }, nil
	default:
		return nil, rpc.RateLimited("Password processing is busy. Try again shortly")
	}
}

// hashPassword and verifyPassword run inside an admitted password command.
func (s *Service) hashPassword(ctx context.Context, password string) (string, error) {
	var salt [passwordSaltBytes]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return "", err
	}
	credential := argonCredential{memory: argonMemory, iterations: argonIterations, lanes: argonLanes, salt: salt[:]}
	key, err := credential.derive(ctx, password)
	if err != nil {
		return "", err
	}
	defer clear(key)
	enc := base64.RawStdEncoding
	header := fmt.Sprintf("v=%d$m=%d,t=%d,p=%d$%s$", argon2.Version, argonMemory, argonIterations, argonLanes,
		enc.EncodeToString(salt[:]))
	if s.passwordPepper == nil {
		return argonPrefix + header + enc.EncodeToString(key), nil
	}
	tag := s.pepperPassword(header, key)
	return pepperedArgonPrefix + header + enc.EncodeToString(tag[:]), nil
}

func (s *Service) pepperPassword(header string, key []byte) [sha256.Size]byte {
	// The header authenticates the scheme version, KDF parameters and salt.
	return macSum(s.passwordPepper[:], []byte(pepperDomain+pepperedArgonPrefix+header), key)
}

type argonCredential struct {
	memory, iterations uint32
	lanes              uint8
	salt, verifier     []byte
	peppered           bool
	header             string
}

func (a argonCredential) derive(ctx context.Context, password string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	passwordBytes := []byte(password)
	defer clear(passwordBytes)
	key := argon2.IDKey(passwordBytes, a.salt, a.iterations, a.memory, a.lanes, passwordKeyBytes)
	if err := ctx.Err(); err != nil {
		clear(key)
		return nil, err
	}
	return key, nil
}

func parseArgon2(stored string) (argonCredential, bool) {
	var argon argonCredential
	if len(stored) > 180 {
		return argon, false
	}
	body, peppered := strings.CutPrefix(stored, pepperedArgonPrefix)
	if !peppered {
		var ok bool
		body, ok = strings.CutPrefix(stored, argonPrefix)
		if !ok {
			return argon, false
		}
	}
	parts := strings.Split(body, "$")
	if len(parts) != 4 || parts[0] != "v=19" {
		return argon, false
	}
	params := strings.Split(parts[1], ",")
	if len(params) != 3 {
		return argon, false
	}
	limits := [3]uint64{argonMemory, argonIterations, argonLanes}
	values := [3]uint64{}
	for i, name := range [3]string{"m=", "t=", "p="} {
		raw, ok := strings.CutPrefix(params[i], name)
		v, err := strconv.ParseUint(raw, 10, 32)
		if !ok || err != nil || v == 0 || v > limits[i] {
			return argon, false
		}
		values[i] = v
	}
	if values[0] < 8*values[2] {
		return argon, false
	}
	enc := base64.RawStdEncoding.Strict()
	salt, err := enc.DecodeString(parts[2])
	if err != nil || len(salt) != passwordSaltBytes {
		return argon, false
	}
	verifier, err := enc.DecodeString(parts[3])
	if err != nil || len(verifier) != passwordKeyBytes {
		return argon, false
	}
	return argonCredential{
		memory: uint32(values[0]), iterations: uint32(values[1]), lanes: uint8(values[2]), salt: salt, verifier: verifier,
		peppered: peppered, header: body[:len(body)-len(parts[3])],
	}, true
}

// v1 trimmed input in its request decoder. Preserve that behavior while
// checking an imported credential and use the same value to rehash it.
func passwordForCredential(stored, submitted string) string {
	switch {
	case strings.HasPrefix(stored, "$2"), strings.HasPrefix(stored, "$csac$md5$"),
		strings.HasPrefix(stored, "$csac$sha256$"), len(stored) == md5.Size*2, len(stored) == sha256.Size*2:
		return strings.TrimSpace(submitted)
	default:
		return submitted
	}
}

func (s *Service) verifyPassword(ctx context.Context, stored, password, username string) (valid, upgrade bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, false, err
	}
	if password == "" {
		return false, false, nil
	}
	switch {
	case strings.HasPrefix(stored, argonPrefix), strings.HasPrefix(stored, pepperedArgonPrefix):
		return s.verifyArgon2(ctx, stored, password)
	case strings.HasPrefix(stored, "$csac$md5$"):
		valid, err = s.verifyBcrypt(ctx, strings.TrimPrefix(stored, "$csac$md5$"), legacyDigest(password, username, md5.Size))
	case strings.HasPrefix(stored, "$csac$sha256$"):
		valid, err = s.verifyBcrypt(ctx, strings.TrimPrefix(stored, "$csac$sha256$"), legacyDigest(password, username, sha256.Size))
	case len(stored) == 2*md5.Size || len(stored) == 2*sha256.Size:
		if _, err := hex.DecodeString(stored); err != nil {
			return false, false, nil
		}
		valid = subtle.ConstantTimeCompare([]byte(strings.ToLower(stored)), []byte(legacyDigest(password, username, len(stored)/2))) == 1
	case strings.HasPrefix(stored, "$2"):
		valid, err = s.verifyBcrypt(ctx, stored, password)
	default:
		// Pay the Argon2 cost for unknown usernames and disabled credentials.
		var salt [passwordSaltBytes]byte
		dummy := argonCredential{memory: argonMemory, iterations: argonIterations, lanes: argonLanes, salt: salt[:]}
		key, err := dummy.derive(ctx, password)
		clear(key)
		return false, false, err
	}
	return valid && err == nil, valid && err == nil, err
}

func (s *Service) verifyArgon2(ctx context.Context, stored, password string) (bool, bool, error) {
	credential, ok := parseArgon2(stored)
	if !ok || (credential.peppered && s.passwordPepper == nil) {
		return false, false, nil
	}
	key, err := credential.derive(ctx, password)
	if err != nil {
		return false, false, err
	}
	defer clear(key)
	candidate := key
	if credential.peppered {
		tag := s.pepperPassword(credential.header, key)
		candidate = tag[:]
	}
	if subtle.ConstantTimeCompare(credential.verifier, candidate) != 1 {
		return false, false, nil
	}
	upgrade := credential.iterations != argonIterations || credential.memory != argonMemory || credential.lanes != argonLanes ||
		(!credential.peppered && s.passwordPepper != nil)
	return true, upgrade, nil
}

func (s *Service) verifyBcrypt(ctx context.Context, stored, password string) (bool, error) {
	// Direct v1 bcrypt accepted up to 72 bytes. In the wrapped formats this
	// argument is the fixed-length hex digest of the full legacy password.
	if len(password) > 72 || len(stored) != 60 {
		return false, nil
	}
	cost, err := bcrypt.Cost([]byte(stored))
	if err != nil || cost > maxLegacyCost {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	passwordBytes := []byte(password)
	defer clear(passwordBytes)
	ok := bcrypt.CompareHashAndPassword([]byte(stored), passwordBytes) == nil
	err = ctx.Err()
	return ok && err == nil, err
}

func legacyDigest(password, username string, size int) string {
	if size == md5.Size {
		sum := md5.Sum([]byte(password))
		return hex.EncodeToString(sum[:])
	}
	sum := sha256.Sum256([]byte(password + username))
	return hex.EncodeToString(sum[:])
}

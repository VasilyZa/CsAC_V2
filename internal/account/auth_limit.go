package account

import (
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"csac/v2/internal/rpc"
)

const (
	authWindow        = time.Minute
	maxAuthRateKeys   = 16_384
	signinPerAddress  = 30
	signinPerIdentity = 10
	signinPerAccount  = 10
	signupPerAddress  = 10
	changesPerAccount = 8
)

type authRateScope uint8

const (
	signinAddress authRateScope = iota
	signinUsername
	signinEmail
	signinUID
	signupAddress
	changeAccount
)

type authRateKey struct {
	scope authRateScope
	value string
}

type authRateWindow struct {
	ends  time.Time
	count uint8
}

// authRateLimits is shared by all command transports in one process. The
// bounded map keeps anonymous attempts from allocating unlimited counters.
type authRateLimits struct {
	mu        sync.Mutex
	nextSweep time.Time
	windows   map[authRateKey]authRateWindow
}

func (l *authRateLimits) allow(now time.Time, scope authRateScope, value string, limit uint8) bool {
	key := authRateKey{scope, value}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.windows == nil {
		l.windows = make(map[authRateKey]authRateWindow)
	}
	if !now.Before(l.nextSweep) {
		for key, window := range l.windows {
			if !now.Before(window.ends) {
				delete(l.windows, key)
			}
		}
		l.nextSweep = now.Add(authWindow)
	}
	window, exists := l.windows[key]
	if !exists && len(l.windows) >= maxAuthRateKeys {
		return false
	}
	if !now.Before(window.ends) {
		window = authRateWindow{ends: now.Add(authWindow)}
	}
	if window.count >= limit {
		return false
	}
	window.count++
	l.windows[key] = window
	return true
}

func (l *authRateLimits) signin(ip, identity string, email bool) error {
	now := time.Now()
	if !l.allow(now, signinAddress, authAddress(ip), signinPerAddress) {
		return authRateExceeded()
	}
	scope := signinUsername
	if email {
		scope = signinEmail
	}
	if !l.allow(now, scope, strings.ToLower(identity), signinPerIdentity) {
		return authRateExceeded()
	}
	return nil
}

func (l *authRateLimits) signinAccount(userID int64) bool {
	return l.allow(time.Now(), signinUID, strconv.FormatInt(userID, 10), signinPerAccount)
}

func (l *authRateLimits) signup(ip string) bool {
	return l.allow(time.Now(), signupAddress, authAddress(ip), signupPerAddress)
}

func (l *authRateLimits) passwordChange(userID int64) bool {
	return l.allow(time.Now(), changeAccount, strconv.FormatInt(userID, 10), changesPerAccount)
}

func authAddress(value string) string {
	ip, err := netip.ParseAddr(value)
	if err != nil {
		return "unknown"
	}
	ip = ip.Unmap()
	if ip.Is6() {
		return netip.PrefixFrom(ip.WithZone(""), 64).Masked().String()
	}
	return ip.String()
}

func authRateExceeded() error {
	return rpc.RateLimited("Too many authentication attempts. Try again in a minute")
}

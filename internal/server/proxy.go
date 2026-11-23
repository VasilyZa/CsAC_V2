package server

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/netip"
)

type proxyClientIP struct{}

// WithTrustedProxy accepts a single client IP from a TLS reverse proxy that
// overwrites both X-Real-IP and X-Csac-Proxy-Token on every forwarded request.
func WithTrustedProxy(next http.Handler, key []byte) http.Handler {
	if len(key) == 0 {
		return next
	}
	token := []byte(base64.StdEncoding.EncodeToString(key))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens := r.Header.Values("X-Csac-Proxy-Token")
		r.Header.Del("X-Csac-Proxy-Token")
		addresses := r.Header.Values("X-Real-IP")
		if len(tokens) == 1 && len(addresses) == 1 && subtle.ConstantTimeCompare([]byte(tokens[0]), token) == 1 {
			if ip, err := netip.ParseAddr(addresses[0]); err == nil && ip.Zone() == "" {
				r = r.WithContext(context.WithValue(r.Context(), proxyClientIP{}, ip.Unmap().String()))
			}
		}
		next.ServeHTTP(w, r)
	})
}

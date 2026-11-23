package server

import (
	"net/http"

	"csac/v2/internal/rpc"
)

func (s *Server) mediaActor(r *http.Request) (rpc.Actor, string, error) {
	if len(r.Header.Values("Authorization")) != 0 {
		return s.requestActor(r)
	}
	cookies := r.CookiesNamed(browserCookieName)
	if len(cookies) == 0 {
		return rpc.Actor{}, "", nil
	}
	if len(r.Header.Values("Origin")) != 0 {
		if s.allowsCookieOrigin(r) {
			return s.authenticateCookie(r, cookies)
		}
	} else if r.Method == http.MethodGet || r.Method == http.MethodHead {
		// Same-origin media requests and top-level download links may omit Origin.
		// Cross-origin embeds should request CORS with credentials.
		site := r.Header.Get("Sec-Fetch-Site")
		navigating := r.Header.Get("Sec-Fetch-Mode") == "navigate"
		if site == "same-origin" || (navigating && (site == "none" || site == "same-site")) {
			return s.authenticateCookie(r, cookies)
		}
	}
	return rpc.Actor{}, "", rpc.Denied("Browser media Origin not allowed")
}

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"csac/v2/internal/netserve"
	"csac/v2/internal/rpc"
)

const browserSessionPath = "/v2/session"
const browserCookieName = "__Host-csac_session"

func (s *Server) browserSession(w http.ResponseWriter, r *http.Request) {
	if !s.allowsCookieOrigin(r) {
		s.writeFailure(w, "", rpc.Denied("Browser session Origin not allowed"))
		return
	}
	req, err := readCommand(w, r)
	if err != nil {
		s.writeFailure(w, req.ID, err)
		return
	}
	switch req.Op {
	case "account.signin", "account.signup", "account.signin.challenge", "account.signup.challenge",
		"account.restore.challenge", "account.restore":
		s.browserGuestCommand(w, r, req)
		return
	case "session.current", "account.logout":
		// These operations use the current browser cookie.
	default:
		s.writeFailure(w, req.ID, rpc.NotFound("Unknown browser session operation"))
		return
	}
	if _, err := rpc.Decode[struct{}](req.Data); err != nil {
		s.writeFailure(w, req.ID, err)
		return
	}
	actor, _, err := s.authenticateCookie(r, r.CookiesNamed(browserCookieName))
	if err != nil && !(req.Op == "account.logout" && unauthenticated(err)) {
		s.writeFailure(w, req.ID, err)
		return
	}
	if req.Op == "session.current" {
		writeBrowserIdentity(w, req.ID, actor.ID)
		return
	}
	resp := rpc.Response{ID: req.ID, Data: json.RawMessage(`{"revoked":0}`)}
	if actor.ID != 0 {
		resp = s.commands.Execute(r.Context(), actor, clientIP(r), req)
	}
	if resp.Error == nil {
		http.SetCookie(w, browserCookie("", time.Unix(0, 0), -1))
	}
	writeCommand(w, resp)
}

func (s *Server) browserGuestCommand(w http.ResponseWriter, r *http.Request, req rpc.Request) {
	resp := s.commands.Execute(r.Context(), rpc.Actor{}, clientIP(r), req)
	if resp.Error != nil {
		resp.Data = nil
		writeCommand(w, resp)
		return
	}
	// Project each success to the fields the browser needs. A raw account reply
	// may contain a session credential or other data outside the browser contract.
	var result struct {
		UserID      int64     `json:"user_id"`
		Token       string    `json:"token"`
		ExpiresAt   time.Time `json:"expires_at"`
		ExpiresIn   int64     `json:"expires_in"`
		ResendAfter int64     `json:"resend_after"`
		Status      string    `json:"status"`
		Rejection   string    `json:"error"`
	}
	invalidResult := errors.New("invalid browser guest command result")
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		s.writeFailure(w, req.ID, invalidResult)
		return
	}
	if result.Rejection == "invalid_email_challenge" {
		// The account command has committed the failed attempt by this point.
		s.writeFailure(w, req.ID, rpc.Error(result.Rejection, "Invalid or expired email challenge", http.StatusUnauthorized))
		return
	}
	if result.Rejection != "" {
		s.writeFailure(w, req.ID, invalidResult)
		return
	}

	switch req.Op {
	case "account.signin", "account.signup":
		remaining := time.Until(result.ExpiresAt)
		if result.UserID > 0 && result.Token != "" && remaining > 0 {
			maxAge := max(1, int(remaining/time.Second))
			http.SetCookie(w, browserCookie(result.Token, result.ExpiresAt, maxAge))
			writeBrowserIdentity(w, req.ID, result.UserID)
			return
		}
	case "account.signin.challenge", "account.signup.challenge", "account.restore.challenge":
		if result.ExpiresIn > 0 && result.ResendAfter > 0 {
			writeBrowserData(w, req.ID, struct {
				ExpiresIn   int64 `json:"expires_in"`
				ResendAfter int64 `json:"resend_after"`
			}{ExpiresIn: result.ExpiresIn, ResendAfter: result.ResendAfter})
			return
		}
	case "account.restore":
		if result.UserID > 0 && result.Status == "active" {
			writeBrowserData(w, req.ID, struct {
				UserID int64  `json:"user_id"`
				Status string `json:"status"`
			}{UserID: result.UserID, Status: result.Status})
			return
		}
	}
	s.writeFailure(w, req.ID, invalidResult)
}

func writeBrowserIdentity(w http.ResponseWriter, id string, userID int64) {
	writeBrowserData(w, id, struct {
		UserID int64 `json:"user_id"`
	}{UserID: userID})
}

func writeBrowserData(w http.ResponseWriter, id string, data any) {
	writeJSON(w, http.StatusOK, struct {
		ID   string `json:"id"`
		Data any    `json:"data"`
	}{ID: id, Data: data})
}

func browserCookie(token string, expires time.Time, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: browserCookieName, Value: token, Path: "/", Expires: expires.UTC(), MaxAge: maxAge,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	}
}

func (s *Server) allowsCookieOrigin(r *http.Request) bool {
	return len(r.Header.Values("Origin")) == 1 && netserve.AllowsOrigin(r, s.sockets.AllowedOrigins)
}

func (s *Server) authenticateCookie(r *http.Request, cookies []*http.Cookie) (rpc.Actor, string, error) {
	if len(cookies) != 1 || cookies[0].Value == "" || len(cookies[0].Value) > 256 {
		return rpc.Actor{}, "", rpc.Unauthenticated()
	}
	return s.authenticate(r, cookies[0].Value)
}

func (s *Server) socketActor(r *http.Request) (rpc.Actor, string, error) {
	if len(r.Header.Values("Authorization")) != 0 {
		return s.requestActor(r)
	}
	cookies := r.CookiesNamed(browserCookieName)
	if len(cookies) == 0 {
		return rpc.Actor{}, "", nil
	}
	if !s.allowsCookieOrigin(r) {
		return rpc.Actor{}, "", rpc.Denied("Browser session Origin not allowed")
	}
	actor, token, err := s.authenticateCookie(r, cookies)
	if unauthenticated(err) {
		// Let the browser see an anonymous hello when the stored session has expired.
		return rpc.Actor{}, "", nil
	}
	return actor, token, err
}

func unauthenticated(err error) bool {
	var fault *rpc.Fault
	return errors.As(err, &fault) && fault.Code == "unauthenticated"
}

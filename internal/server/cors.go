package server

import (
	"net/http"
	"slices"
	"strings"

	"csac/v2/internal/netserve"
	"csac/v2/internal/stickerart"
)

var commandMethods = []string{http.MethodPost}
var mediaMethods = []string{http.MethodGet, http.MethodHead, http.MethodPut}
var releaseMethods = []string{http.MethodGet, http.MethodHead}

const mediaPrefix = "/v2/media/"

func (s *Server) cors(w http.ResponseWriter, r *http.Request) bool {
	var methods []string
	mediaPath := strings.HasPrefix(r.URL.Path, mediaPrefix)
	credentials := r.URL.Path == browserSessionPath || mediaPath
	switch {
	case r.URL.Path == "/v2/call" || r.URL.Path == browserSessionPath:
		methods = commandMethods
	case strings.HasPrefix(r.URL.Path, stickerart.Prefix):
		methods = releaseMethods
	case mediaPath:
		methods = mediaMethods
	case strings.HasPrefix(r.URL.Path, "/v2/releases/"):
		methods = releaseMethods
	default:
		return false
	}

	headers := w.Header()
	headers.Add("Vary", "Origin")
	origin := r.Header.Get("Origin")
	allowed := origin != "" && netserve.AllowsOrigin(r, s.sockets.AllowedOrigins)
	if r.Method != http.MethodOptions {
		if allowed {
			headers.Set("Access-Control-Allow-Origin", origin)
			if credentials {
				headers.Set("Access-Control-Allow-Credentials", "true")
			}
			headers.Set("Access-Control-Expose-Headers", "Content-Disposition, Content-Range, ETag, Location")
		}
		return false
	}

	headers.Add("Vary", "Access-Control-Request-Method")
	headers.Add("Vary", "Access-Control-Request-Headers")
	if origin == "" && len(r.Header.Values("Origin")) == 0 {
		headers.Set("Allow", strings.Join(methods, ", "))
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	if !allowed || !allowedBrowserHeaders(r.Header.Values("Access-Control-Request-Headers")) {
		http.Error(w, "Cross-origin request is not allowed", http.StatusForbidden)
		return true
	}
	method := r.Header.Get("Access-Control-Request-Method")
	if !slices.Contains(methods, method) {
		headers.Set("Allow", strings.Join(methods, ", "))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	headers.Set("Access-Control-Allow-Origin", origin)
	if credentials {
		headers.Set("Access-Control-Allow-Credentials", "true")
	}
	headers.Set("Access-Control-Allow-Methods", strings.Join(methods, ", "))
	headers.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Range")
	headers.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
	return true
}

func allowedBrowserHeaders(values []string) bool {
	for _, line := range values {
		for _, header := range strings.Split(line, ",") {
			switch strings.ToLower(strings.TrimSpace(header)) {
			case "authorization", "content-type", "range":
			default:
				return false
			}
		}
	}
	return true
}

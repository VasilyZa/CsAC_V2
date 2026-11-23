package netserve

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gobwas/ws"
	"golang.org/x/net/http/httpguts"
)

// SocketOptions controls admission and message limits for an Upgrade.
// Zero values allow same-origin requests and requests without Origin.
type SocketOptions struct {
	// AllowedOrigins adds exact HTTP(S) origins to the same-origin policy.
	AllowedOrigins []string
	// MaxMessageBytes provides the fallback for either unset directional limit.
	// Zero uses DefaultMaxMessageBytes as the fallback.
	MaxMessageBytes int
	// MaxReadMessageBytes bounds an incoming text message across all fragments.
	// Zero uses the fallback.
	MaxReadMessageBytes int
	// MaxWriteMessageBytes bounds an outgoing text message. Zero uses the fallback.
	MaxWriteMessageBytes int
	// WriteTimeout bounds writes and automatic pong responses. Zero uses five seconds.
	WriteTimeout time.Duration
}

// Upgrade accepts a CsAC WebSocket with the default socket options. An error
// writes a rejection when possible. The handler must stay alive for the socket
// lifetime, including on HTTP/2 and HTTP/3 where the stream belongs to it.
func Upgrade(w http.ResponseWriter, r *http.Request) (*Socket, error) {
	return (SocketOptions{}).Upgrade(w, r)
}

// Upgrade accepts a CsAC WebSocket with these options.
func (o SocketOptions) Upgrade(w http.ResponseWriter, r *http.Request) (*Socket, error) {
	if o.MaxMessageBytes < 0 || o.MaxReadMessageBytes < 0 || o.MaxWriteMessageBytes < 0 || o.WriteTimeout < 0 {
		return reject(w, http.StatusInternalServerError, "invalid WebSocket limits")
	}
	if o.MaxMessageBytes == 0 {
		o.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if o.MaxReadMessageBytes == 0 {
		o.MaxReadMessageBytes = o.MaxMessageBytes
	}
	if o.MaxWriteMessageBytes == 0 {
		o.MaxWriteMessageBytes = o.MaxMessageBytes
	}
	if o.WriteTimeout == 0 {
		o.WriteTimeout = defaultWriteTimeout
	}
	if err := validateUpgrade(w, r, o.AllowedOrigins); err != nil {
		return nil, err
	}

	scope, _ := r.Context().Value(socketScopeKey{}).(*socketScope)
	if scope != nil {
		if scope.reserved {
			return nil, errors.New("netserve: request already upgraded")
		}
		if !scope.group.reserve() {
			return reject(w, http.StatusServiceUnavailable, "server shutting down")
		}
		scope.reserved = true
	}

	var wire frameStream
	if r.ProtoMajor == 1 {
		headers := w.Header().Clone()
		for _, key := range []string{"Connection", "Upgrade", "Sec-WebSocket-Accept", "Sec-WebSocket-Protocol", "Sec-WebSocket-Extensions", "Content-Length", "Transfer-Encoding"} {
			headers.Del(key)
		}
		upgrader := ws.HTTPUpgrader{
			Timeout: min(o.WriteTimeout, handshakeTimeout),
			Header:  headers,
			Protocol: func(value string) bool {
				return value == Subprotocol
			},
		}
		// Pass normalized handshake fields to the upgrader, including when
		// the client split the Connection or subprotocol fields across lines.
		normalized := r.Clone(r.Context())
		normalized.Header.Set("Connection", "Upgrade")
		normalized.Header.Set("Sec-WebSocket-Protocol", Subprotocol)
		conn, buffered, _, err := upgrader.Upgrade(normalized, w)
		if err != nil {
			if conn != nil {
				_ = conn.Close()
			}
			return nil, err
		}
		wire = tcpStream(conn, buffered.Reader)
	} else {
		ctl := http.NewResponseController(w)
		if err := ctl.SetReadDeadline(time.Time{}); err != nil {
			return reject(w, http.StatusInternalServerError, "response writer lacks WebSocket read deadlines")
		}
		if err := ctl.SetWriteDeadline(time.Now().Add(min(o.WriteTimeout, handshakeTimeout))); err != nil {
			return reject(w, http.StatusInternalServerError, "response writer lacks WebSocket write deadlines")
		}
		for _, key := range []string{"Connection", "Upgrade", "Sec-WebSocket-Accept", "Sec-WebSocket-Extensions", "Content-Length", "Transfer-Encoding"} {
			w.Header().Del(key)
		}
		w.Header().Set("Sec-WebSocket-Protocol", Subprotocol)
		w.WriteHeader(http.StatusOK)
		if err := ctl.Flush(); err != nil {
			return nil, err
		}
		if err := ctl.SetWriteDeadline(time.Time{}); err != nil {
			return nil, err
		}
		// HTTP/2 and HTTP/3 already allow full duplex. Let the HTTP server
		// finish the stream after the handler returns, including its last DATA.
		wire = frameStream{
			reader: r.Body, writer: w, flush: ctl.Flush,
			interruptRead:  func() { _ = ctl.SetReadDeadline(expiredDeadline) },
			interruptWrite: func() { _ = ctl.SetWriteDeadline(expiredDeadline) },
		}
	}
	socket := newSocket(wire, false, o.MaxReadMessageBytes, o.MaxWriteMessageBytes, o.WriteTimeout)
	if scope != nil {
		scope.attach(socket)
	}
	return socket, nil
}

func reject(w http.ResponseWriter, status int, message string) (*Socket, error) {
	http.Error(w, message, status)
	return nil, errors.New("netserve: " + message)
}

func validateUpgrade(w http.ResponseWriter, r *http.Request, allowedOrigins []string) error {
	var method string
	switch r.ProtoMajor {
	case 1:
		method = http.MethodGet
	case 2, 3:
		method = http.MethodConnect
	default:
		_, err := reject(w, http.StatusHTTPVersionNotSupported, "unsupported WebSocket HTTP version")
		return err
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		_, err := reject(w, http.StatusMethodNotAllowed, "invalid WebSocket method")
		return err
	}
	badProtocol := false
	switch r.ProtoMajor {
	case 1:
		keys := r.Header.Values("Sec-WebSocket-Key")
		badKey := len(keys) != 1
		if !badKey {
			key, err := base64.StdEncoding.Strict().DecodeString(keys[0])
			badKey = err != nil || len(key) != 16
		}
		upgrades := r.Header.Values("Upgrade")
		badProtocol = r.ProtoMinor != 1 || len(upgrades) != 1 || !strings.EqualFold(upgrades[0], "websocket") ||
			!httpguts.HeaderValuesContainsToken(r.Header.Values("Connection"), "upgrade") || badKey || r.ContentLength != 0 ||
			len(r.TransferEncoding) != 0 || len(r.Header.Values(":protocol")) != 0
	case 2, 3:
		protocol := r.Header.Values(":protocol")
		validH2 := r.ProtoMajor == 2 && len(protocol) == 1 && protocol[0] == "websocket"
		validH3 := r.ProtoMajor == 3 && r.Proto == "websocket" && len(protocol) == 0
		badProtocol = !(validH2 || validH3) || r.Body == nil || r.Body == http.NoBody || r.ContentLength > 0
		for _, key := range []string{"Connection", "Upgrade", "Sec-WebSocket-Key", "Sec-WebSocket-Accept"} {
			badProtocol = badProtocol || len(r.Header.Values(key)) != 0
		}
	}
	if badProtocol {
		_, err := reject(w, http.StatusBadRequest, "invalid WebSocket handshake")
		return err
	}
	if versions := r.Header.Values("Sec-WebSocket-Version"); len(versions) != 1 || versions[0] != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		_, err := reject(w, http.StatusUpgradeRequired, "WebSocket version 13 required")
		return err
	}
	if !offersSubprotocol(r.Header.Values("Sec-WebSocket-Protocol")) {
		_, err := reject(w, http.StatusBadRequest, "WebSocket subprotocol csac.v2 required")
		return err
	}
	if !AllowsOrigin(r, allowedOrigins) {
		_, err := reject(w, http.StatusForbidden, "WebSocket Origin not allowed")
		return err
	}
	return nil
}

func offersSubprotocol(values []string) bool {
	offered := false
	for _, value := range values {
		for token := range strings.SplitSeq(value, ",") {
			token = strings.TrimSpace(token)
			if !httpguts.ValidHeaderFieldName(token) {
				return false
			}
			offered = offered || token == Subprotocol
		}
	}
	return offered
}

type origin struct{ scheme, host, port string }

func parseOrigin(raw string) (origin, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		u.Path != "" || strings.ContainsAny(raw, "?#") || strings.HasSuffix(u.Host, ":") {
		return origin{}, false
	}
	host := strings.ToLower(u.Hostname())
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	} else {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 0 {
			return origin{}, false
		}
		port = strconv.FormatUint(n, 10)
	}
	return origin{u.Scheme, host, port}, true
}

// AllowsOrigin applies the same-origin and explicit allowlist policy.
func AllowsOrigin(r *http.Request, allowed []string) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 {
		return false
	}
	given, ok := parseOrigin(values[0])
	if !ok {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if local, ok := parseOrigin(scheme + "://" + r.Host); ok && local == given {
		return true
	}
	for _, value := range allowed {
		if allowedOrigin, ok := parseOrigin(value); ok && allowedOrigin == given {
			return true
		}
	}
	return false
}

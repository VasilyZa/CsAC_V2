package netserve

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gobwas/ws"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http/httpguts"
)

// Dial establishes a CsAC WebSocket over exactly HTTP/1.1, HTTP/2, or HTTP/3
// for proto 1, 2, or 3. The endpoint must use wss:// or https://. The context
// governs the handshake. Each Socket owns its connection until closed.
func Dial(ctx context.Context, endpoint string, proto int, tlsConfig *tls.Config, headers http.Header) (*Socket, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" || u.User != nil || strings.Contains(endpoint, "#") || (u.Scheme != "https" && u.Scheme != "wss") {
		return nil, errors.New("netserve: expected a wss:// or https:// WebSocket URL")
	}
	if proto != 1 && proto != 2 && proto != 3 {
		return nil, fmt.Errorf("netserve: unsupported dial protocol %d", proto)
	}
	headers, err = dialHeaders(headers)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	var wire frameStream
	switch proto {
	case 1:
		u.Scheme = "wss"
		wire, err = dialH1(ctx, u, tlsForClient(tlsConfig, "http/1.1"), headers)
	case 2:
		u.Scheme = "https"
		wire, err = dialH2(ctx, u, tlsForClient(tlsConfig, "h2"), connectHeaders(headers))
	case 3:
		u.Scheme = "https"
		wire, err = dialH3(ctx, u, tlsForClient(tlsConfig, http3.NextProtoH3), connectHeaders(headers))
	}
	if err != nil {
		return nil, fmt.Errorf("netserve: dial HTTP/%d WebSocket: %w", proto, err)
	}
	return newSocket(wire, true, DefaultMaxMessageBytes+ResponseEnvelopeBytes, DefaultMaxMessageBytes, defaultWriteTimeout), nil
}

func dialHeaders(original http.Header) (http.Header, error) {
	headers := make(http.Header, len(original)+2)
	for key, values := range original {
		key = http.CanonicalHeaderKey(key)
		if !httpguts.ValidHeaderFieldName(key) || strings.HasPrefix(key, "Sec-Websocket-") {
			return nil, fmt.Errorf("netserve: invalid or reserved dial header %q", key)
		}
		switch key {
		case "Connection", "Upgrade", "Host", "Content-Length", "Transfer-Encoding", "Trailer", "Te", "Keep-Alive", "Proxy-Connection":
			return nil, fmt.Errorf("netserve: reserved dial header %q", key)
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return nil, fmt.Errorf("netserve: invalid value for dial header %q", key)
			}
			headers.Add(key, value)
		}
	}
	return headers, nil
}

func connectHeaders(headers http.Header) http.Header {
	headers.Set("Sec-WebSocket-Version", "13")
	headers.Set("Sec-WebSocket-Protocol", Subprotocol)
	return headers
}

func tlsForClient(base *tls.Config, alpn string) *tls.Config {
	if base == nil {
		base = &tls.Config{}
	}
	cfg := base.Clone()
	cfg.NextProtos = []string{alpn}
	return cfg
}

func dialAddress(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port)
}

func dialH1(ctx context.Context, u *url.URL, cfg *tls.Config, headers http.Header) (frameStream, error) {
	dialer := ws.Dialer{
		TLSConfig: cfg,
		Protocols: []string{Subprotocol},
		Header:    ws.HandshakeHeaderHTTP(headers),
	}
	conn, buffered, hs, err := dialer.Dial(ctx, u.String())
	if err != nil {
		return frameStream{}, err
	}
	if hs.Protocol != Subprotocol || len(hs.Extensions) != 0 {
		_ = conn.Close()
		return frameStream{}, errors.New("server did not negotiate csac.v2 without extensions")
	}
	reader := io.Reader(conn)
	if buffered != nil {
		reader = buffered
	}
	return tcpStream(conn, reader), nil
}

func dialH3(ctx context.Context, u *url.URL, cfg *tls.Config, headers http.Header) (_ frameStream, err error) {
	if cfg.ServerName == "" {
		cfg.ServerName = u.Hostname()
	}
	conn, err := quic.DialAddr(ctx, dialAddress(u), cfg, &quic.Config{
		HandshakeIdleTimeout: handshakeTimeout,
		MaxIdleTimeout:       idleTimeout,
	})
	if err != nil {
		return frameStream{}, err
	}
	closeConn := func() error { return conn.CloseWithError(quic.ApplicationErrorCode(http3.ErrCodeNoError), "") }
	defer func() {
		if err != nil {
			_ = closeConn()
		}
	}()
	transport := &http3.Transport{DisableCompression: true, MaxResponseHeaderBytes: maxHeaderBytes}
	client := transport.NewClientConn(conn)
	select {
	case <-ctx.Done():
		return frameStream{}, ctx.Err()
	case <-conn.Context().Done():
		return frameStream{}, context.Cause(conn.Context())
	case <-client.ReceivedSettings():
	}
	if !client.Settings().EnableExtendedConnect {
		return frameStream{}, errors.New("peer did not enable HTTP/3 Extended CONNECT")
	}
	stream, err := client.OpenRequestStream(ctx)
	if err != nil {
		return frameStream{}, err
	}
	req := &http.Request{Method: http.MethodConnect, URL: u, Proto: "websocket", Header: headers}
	err = runIO(ctx, func() { _ = closeConn() }, func() error {
		if err := stream.SendRequestHeader(req); err != nil {
			return err
		}
		resp, err := stream.ReadResponse()
		if err != nil {
			return err
		}
		if resp.ProtoMajor != 3 {
			return fmt.Errorf("unexpected response protocol %s", resp.Proto)
		}
		return checkConnectResponse(resp.StatusCode, resp.Header)
	})
	if err != nil {
		return frameStream{}, err
	}

	return frameStream{
		reader: stream, writer: stream,
		interruptRead:  func() { _ = stream.SetReadDeadline(expiredDeadline) },
		interruptWrite: func() { _ = stream.SetWriteDeadline(expiredDeadline) },
		close: func() error {
			// Give the final DATA and FIN time to reach the peer before tearing
			// down the QUIC connection. The drain is bounded in bytes and time.
			if stream.Close() == nil {
				_ = stream.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
				_, _ = io.CopyN(io.Discard, stream, 16<<10)
			}
			return closeConn()
		},
	}, nil
}

func checkConnectResponse(status int, headers http.Header) error {
	if status != http.StatusOK {
		return fmt.Errorf("Extended CONNECT returned HTTP %d", status)
	}
	values := headers.Values("Sec-WebSocket-Protocol")
	if len(values) != 1 || values[0] != Subprotocol || len(headers.Values("Sec-WebSocket-Extensions")) != 0 {
		return errors.New("server did not negotiate csac.v2 without extensions")
	}
	for _, key := range []string{"Connection", "Upgrade", "Sec-WebSocket-Accept", "Content-Length", "Content-Encoding"} {
		if len(headers.Values(key)) != 0 {
			return fmt.Errorf("unexpected Extended CONNECT response header %s", key)
		}
	}
	return nil
}

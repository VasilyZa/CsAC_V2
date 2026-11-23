// Package netserve serves the CsAC transport over TLS on TCP and QUIC on UDP.
package netserve

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

const (
	handshakeTimeout = 5 * time.Second
	idleTimeout      = 2 * time.Minute
	maxHeaderBytes   = 64 << 10
)

// ErrHTTP2ConnectDisabled means Go was started without the HTTP/2 Extended
// CONNECT setting. Changing GODEBUG after startup does not enable it.
var ErrHTTP2ConnectDisabled = errors.New("netserve: start the process with GODEBUG=http2xconnect=1")

var http2ConnectAtStartup = func() bool {
	var enabled bool
	for field := range strings.SplitSeq(os.Getenv("GODEBUG"), ",") {
		if key, value, ok := strings.Cut(field, "="); ok && key == "http2xconnect" {
			enabled = value == "1"
		}
	}
	return enabled
}()

// Config supplies one address and the TLS credentials for both listeners.
type Config struct {
	Addr      string
	TLSConfig *tls.Config
	// ShutdownTimeout bounds graceful drain. Zero uses five seconds.
	ShutdownTimeout time.Duration
}

// Server is a running TCP and QUIC server created by Listen.
type Server struct {
	addr string
	done chan struct{}
	err  error
}

// Addr returns the bound address shared by TCP and UDP.
func (s *Server) Addr() string { return s.addr }

// Wait returns the serve or shutdown error, if any, after the listeners close.
func (s *Server) Wait() error {
	<-s.done
	return s.err
}

// Serve binds the address and serves until ctx is canceled or a listener fails.
func Serve(ctx context.Context, cfg Config, handler http.Handler) error {
	s, err := Listen(ctx, cfg, handler)
	if err != nil {
		return err
	}
	return s.Wait()
}

// Listen binds both listeners before returning and serves handler on each.
// Cancel ctx and call Wait to drain the server. Addr accepts an ephemeral port.
func Listen(ctx context.Context, cfg Config, handler http.Handler) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !http2ConnectAtStartup {
		return nil, ErrHTTP2ConnectDisabled
	}
	if cfg.TLSConfig == nil || (len(cfg.TLSConfig.Certificates) == 0 && cfg.TLSConfig.GetCertificate == nil && cfg.TLSConfig.GetConfigForClient == nil) {
		return nil, errors.New("netserve: TLS server certificate required")
	}
	if cfg.TLSConfig.MaxVersion != 0 && cfg.TLSConfig.MaxVersion < tls.VersionTLS13 || cfg.TLSConfig.MinVersion > tls.VersionTLS13 {
		return nil, errors.New("netserve: TLS configuration must allow TLS 1.3 for QUIC")
	}
	if cfg.ShutdownTimeout < 0 {
		return nil, errors.New("netserve: negative shutdown timeout")
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 5 * time.Second
	}
	if handler == nil {
		handler = http.DefaultServeMux
	}

	tcp, udp, err := listenTCPUDP(ctx, cfg.Addr)
	if err != nil {
		return nil, err
	}
	addr := tcp.Addr().String()
	qt := &quic.Transport{Conn: udp}
	ql, err := qt.Listen(http3.ConfigureTLSConfig(cfg.TLSConfig), &quic.Config{
		Allow0RTT:            false,
		HandshakeIdleTimeout: handshakeTimeout,
		MaxIdleTimeout:       idleTimeout,
	})
	if err != nil {
		qt.Close()
		udp.Close()
		tcp.Close()
		return nil, fmt.Errorf("netserve: listen QUIC: %w", err)
	}

	group := newSocketGroup()
	_, port, _ := net.SplitHostPort(addr)
	advertisement := fmt.Sprintf(`h3=":%s"; ma=86400`, port)
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor < 3 {
			w.Header().Set("Alt-Svc", advertisement)
		}
		scope := &socketScope{group: group}
		requestCtx, cancel := context.WithCancel(context.WithValue(r.Context(), socketScopeKey{}, scope))
		scope.cancel = cancel
		defer scope.finish()
		handler.ServeHTTP(w, r.WithContext(requestCtx))
	})
	var versions http.Protocols
	versions.SetHTTP1(true)
	versions.SetHTTP2(true)
	h12 := &http.Server{
		Handler:           wrapped,
		TLSConfig:         tcpTLS(cfg.TLSConfig),
		Protocols:         &versions,
		ReadHeaderTimeout: handshakeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	h3 := &http3.Server{Handler: wrapped, IdleTimeout: idleTimeout, MaxHeaderBytes: maxHeaderBytes}
	s := &Server{addr: addr, done: make(chan struct{})}
	go s.run(ctx, cfg.ShutdownTimeout, group, tcp, udp, qt, ql, h12, h3)
	return s, nil
}

func listenTCPUDP(ctx context.Context, addr string) (net.Listener, net.PacketConn, error) {
	_, port, _ := net.SplitHostPort(addr)
	ephemeral := port == "" || port == "0"
	lc := net.ListenConfig{}
	for attempt := 1; ; attempt++ {
		tcp, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			return nil, nil, fmt.Errorf("netserve: listen TCP: %w", err)
		}
		udp, err := lc.ListenPacket(ctx, "udp", tcp.Addr().String())
		if err == nil {
			return tcp, udp, nil
		}
		tcp.Close()
		// The OS assigns ephemeral TCP and UDP ports independently.
		if !ephemeral || attempt == 8 || !errors.Is(err, syscall.EADDRINUSE) {
			return nil, nil, fmt.Errorf("netserve: listen UDP: %w", err)
		}
	}
}

func tcpTLS(base *tls.Config) *tls.Config {
	cfg := base.Clone()
	cfg.NextProtos = []string{"h2", "http/1.1"}
	if selectConfig := cfg.GetConfigForClient; selectConfig != nil {
		cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			selected, err := selectConfig(hello)
			if selected == nil || err != nil {
				return selected, err
			}
			selected = selected.Clone()
			selected.NextProtos = cfg.NextProtos
			return selected, nil
		}
	}
	return cfg
}

func (s *Server) run(ctx context.Context, timeout time.Duration, group *socketGroup, tcp net.Listener, udp net.PacketConn, qt *quic.Transport, ql *quic.Listener, h12 *http.Server, h3 *http3.Server) {
	defer close(s.done)
	defer udp.Close()
	defer qt.Close()
	defer ql.Close()
	defer tcp.Close()

	serveErr := make(chan error, 2)
	go func() { serveErr <- fmt.Errorf("netserve: serve TCP: %w", h12.ServeTLS(tcp, "", "")) }()
	go func() { serveErr <- fmt.Errorf("netserve: serve QUIC: %w", h3.ServeListener(ql)) }()
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		s.err = err
	}

	group.stop()
	drain, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	results := make(chan error, 3)
	go func() { results <- h12.Shutdown(drain) }()
	go func() { results <- h3.Shutdown(drain) }()
	go func() { group.handlers.Wait(); results <- nil }()
	for range 3 {
		select {
		case err := <-results:
			s.err = errors.Join(s.err, err)
		case <-drain.Done():
			s.err = errors.Join(s.err, fmt.Errorf("netserve: drain: %w", drain.Err()))
			_ = h12.Close()
			return
		}
	}
	if s.err != nil {
		_ = h12.Close()
	}
}

type socketScopeKey struct{}

type socketGroup struct {
	mu       sync.Mutex
	closing  bool
	handlers sync.WaitGroup
	ctx      context.Context
	cancel   context.CancelFunc
}

func newSocketGroup() *socketGroup {
	ctx, cancel := context.WithCancel(context.Background())
	return &socketGroup{ctx: ctx, cancel: cancel}
}

func (g *socketGroup) reserve() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closing {
		return false
	}
	g.handlers.Add(1)
	return true
}

func (g *socketGroup) stop() {
	g.mu.Lock()
	g.closing = true
	g.mu.Unlock()
	g.cancel()
}

type socketScope struct {
	group      *socketGroup
	cancel     context.CancelFunc
	reserved   bool
	socket     *Socket
	stopSocket func() bool
}

func (s *socketScope) attach(socket *Socket) {
	s.socket = socket
	socket.onClose = s.cancel
	s.stopSocket = context.AfterFunc(s.group.ctx, func() {
		_ = socket.Close(1001, "server shutting down")
	})
}

func (s *socketScope) finish() {
	if s.socket != nil {
		s.stopSocket()
		_ = s.socket.Close(1000, "")
	}
	s.cancel()
	if s.reserved {
		s.group.handlers.Done()
	}
}

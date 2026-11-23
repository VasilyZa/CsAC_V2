// Package server exposes the command service over HTTP and WebSocket.
package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"csac/v2/internal/netserve"
	"csac/v2/internal/realtime"
	"csac/v2/internal/rpc"
	"csac/v2/internal/stickerart"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Media interface {
	HTTP(http.ResponseWriter, *http.Request, rpc.Actor)
}
type Server struct {
	pool     *pgxpool.Pool
	commands *rpc.Registry
	auth     rpc.Authenticator
	hub      *realtime.Hub
	media    Media
	releases http.Handler
	sockets  netserve.SocketOptions
	slots    chan struct{}
	outbound outboundBudget
	mux      *http.ServeMux
}

func New(pool *pgxpool.Pool, commands *rpc.Registry, auth rpc.Authenticator, hub *realtime.Hub, media Media, releases http.Handler, maxSockets int, origins []string, logger *slog.Logger) *Server {
	sockets := netserve.SocketOptions{
		AllowedOrigins:       origins,
		MaxReadMessageBytes:  rpc.MaxCommandBytes,
		MaxWriteMessageBytes: rpc.MaxResponseBytes + netserve.ResponseEnvelopeBytes,
	}
	s := &Server{pool: pool, commands: commands, auth: auth, hub: hub, media: media, releases: releases, slots: make(chan struct{}, maxSockets), mux: http.NewServeMux(), sockets: sockets}
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	s.mux.HandleFunc("GET /readyz", s.ready)
	s.mux.HandleFunc("POST /v2/call", s.call)
	s.mux.HandleFunc("POST "+browserSessionPath, s.browserSession)
	s.mux.HandleFunc("/v2/socket", s.websocket)
	s.mux.HandleFunc(mediaPrefix, s.file)
	s.mux.HandleFunc("GET /v2/assets/default-avatar.svg", serveDefaultAvatar)
	s.mux.HandleFunc(stickerart.Prefix, stickerart.Serve)
	if releases != nil {
		s.mux.Handle("/v2/releases/", releases)
		if deployment, ok := releases.(interface{ WebHandler() http.Handler }); ok {
			if web := deployment.WebHandler(); web != nil {
				s.mux.Handle("/", web)
			}
		}
	}
	commands.Register("system.time", rpc.Method{Access: rpc.Public, Handle: readServerTime})
	commands.Register("system.operations", rpc.Method{Access: rpc.Public, Handle: func(ctx context.Context, call rpc.Call, data json.RawMessage) (any, error) {
		if _, err := rpc.Decode[struct{}](data); err != nil {
			return nil, err
		}
		ops := append(commands.Operations(),
			rpc.Operation{Name: "session.attach", Access: "public", WebSocketOnly: true},
			rpc.Operation{Name: "events.confirm", Access: "member", WebSocketOnly: true},
			rpc.Operation{Name: "presence.watch", Access: "member", WebSocketOnly: true},
		)
		slices.SortFunc(ops, func(a, b rpc.Operation) int { return strings.Compare(a.Name, b.Name) })
		return ops, nil
	}})
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if s.cors(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.pool.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) call(w http.ResponseWriter, r *http.Request) {
	req, err := readCommand(w, r)
	if err != nil {
		s.writeFailure(w, req.ID, err)
		return
	}
	actor, _, err := s.requestActor(r)
	if err != nil {
		s.writeFailure(w, req.ID, err)
		return
	}
	if actor.ID > 0 {
		s.hub.Touch(actor.ID)
	}
	writeCommand(w, s.commands.Execute(r.Context(), actor, clientIP(r), req))
}

func readCommand(w http.ResponseWriter, r *http.Request) (rpc.Request, error) {
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/json" {
		return rpc.Request{}, rpc.Error("content_type", "Use application/json", http.StatusUnsupportedMediaType)
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, rpc.MaxCommandBytes))
	if err != nil {
		return rpc.Request{}, rpc.Invalid("Request body is unreadable or exceeds its limit")
	}
	return rpc.Parse(raw)
}

func writeCommand(w http.ResponseWriter, resp rpc.Response) {
	status := http.StatusOK
	if resp.Error != nil {
		status = resp.Error.Status
	}
	writeJSON(w, status, resp)
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	if s.media == nil {
		http.NotFound(w, r)
		return
	}
	actor, _, err := s.mediaActor(r)
	if err != nil {
		s.writeFailure(w, "", err)
		return
	}
	if actor.ID > 0 {
		s.hub.Touch(actor.ID)
	}
	s.media.HTTP(w, r, actor)
}

func (s *Server) requestActor(r *http.Request) (rpc.Actor, string, error) {
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		return rpc.Actor{}, "", nil
	}
	if len(values) != 1 {
		return rpc.Actor{}, "", rpc.Unauthenticated()
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 256 {
		return rpc.Actor{}, "", rpc.Unauthenticated()
	}
	return s.authenticate(r, parts[1])
}

func (s *Server) authenticate(r *http.Request, token string) (rpc.Actor, string, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	actor, err := s.auth.Authenticate(ctx, token)
	return actor, token, err
}

func (s *Server) writeFailure(w http.ResponseWriter, id string, err error) {
	resp := s.commands.Failed(id, err)
	writeJSON(w, resp.Error.Status, resp)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func clientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(proxyClientIP{}).(string); ok {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

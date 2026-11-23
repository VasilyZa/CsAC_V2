package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"csac/v2/internal/netserve"
	"csac/v2/internal/realtime"
	"csac/v2/internal/rpc"
)

type peer struct {
	cancel     context.CancelFunc
	out        chan []byte
	budget     *outboundBudget
	queueMu    sync.Mutex
	closed     bool
	queued     atomic.Int64
	identity   atomic.Pointer[peerIdentity]
	verify     atomic.Bool
	overloaded atomic.Bool
}
type peerIdentity struct {
	user  int64
	token string
	chat  bool
}

func (s *Server) websocket(w http.ResponseWriter, r *http.Request) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		s.writeFailure(w, "", rpc.Error("busy", "Socket limit reached", http.StatusServiceUnavailable))
		return
	}
	actor, token, err := s.socketActor(r)
	if err != nil {
		s.writeFailure(w, "", err)
		return
	}
	socket, err := s.sockets.Upgrade(w, r)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	p := s.newPeer(cancel)
	p.identity.Store(&peerIdentity{user: actor.ID, token: token, chat: actor.Chat})
	done := make(chan struct{})
	go func() { defer close(done); defer cancel(); s.writePeer(ctx, socket, p) }()
	defer func() {
		cancel()
		<-done
		p.discardQueue()
		if p.overloaded.Load() {
			_ = socket.Close(1013, "Reconnect and replay")
		} else {
			_ = socket.Close(1000, "")
		}
	}()
	var unsubscribe, stopPresence func()
	var confirmed int64
	defer func() {
		if stopPresence != nil {
			stopPresence()
		}
		if unsubscribe != nil {
			unsubscribe()
		}
	}()
	if !s.hello(ctx, p, actor) {
		return
	}
	if actor.ID > 0 {
		s.hub.Touch(actor.ID)
	}
	if actor.Chat {
		unsubscribe = s.hub.Subscribe(actor.ID, p.event)
	}
	budget := bucket{tokens: 120, last: time.Now()}
	for ctx.Err() == nil {
		readCtx := ctx
		finish := func() {}
		if token == "" {
			readCtx, finish = context.WithDeadline(ctx, budget.started().Add(30*time.Second))
		}
		raw, err := socket.Read(readCtx)
		finish()
		if err != nil {
			return
		}
		req, err := rpc.Parse(raw)
		if !budget.allow(token != "") {
			p.send(s.commands.Failed(req.ID, rpc.RateLimited("Socket command rate exceeded")))
			continue
		}
		if err != nil {
			p.send(s.commands.Failed(req.ID, err))
			continue
		}
		if token != "" {
			checkCtx, done := context.WithTimeout(ctx, 3*time.Second)
			current, e := s.auth.Authenticate(checkCtx, token)
			done()
			if e != nil {
				s.writeImmediate(ctx, socket, s.commands.Failed(req.ID, e))
				return
			}
			if current.Chat != actor.Chat {
				if stopPresence != nil {
					stopPresence()
					stopPresence = nil
				}
				if unsubscribe != nil {
					unsubscribe()
					unsubscribe = nil
				}
				p.identity.Store(&peerIdentity{user: current.ID, token: token, chat: current.Chat})
				if current.Chat {
					unsubscribe = s.hub.Subscribe(current.ID, p.event)
				}
			}
			actor = current
			s.hub.Touch(actor.ID)
		}
		if req.Op == "session.attach" {
			if token != "" {
				p.send(s.commands.Failed(req.ID, rpc.Conflict("This socket already has a session")))
				continue
			}
			input, e := rpc.Decode[struct {
				Token string `json:"token"`
			}](req.Data)
			if e != nil || len(input.Token) > 256 || input.Token == "" {
				p.send(s.commands.Failed(req.ID, rpc.Invalid("Session token required")))
				continue
			}
			checkCtx, finish := context.WithTimeout(ctx, 3*time.Second)
			current, e := s.auth.Authenticate(checkCtx, input.Token)
			finish()
			if e != nil {
				p.send(s.commands.Failed(req.ID, e))
				continue
			}
			info, err := s.identityData(ctx, current)
			if err != nil {
				p.send(s.commands.Failed(req.ID, err))
				continue
			}
			actor, token = current, input.Token
			s.hub.Touch(actor.ID)
			p.identity.Store(&peerIdentity{user: actor.ID, token: token, chat: actor.Chat})
			body, _ := json.Marshal(info)
			p.send(rpc.Response{ID: req.ID, Data: body})
			if actor.Chat {
				unsubscribe = s.hub.Subscribe(actor.ID, p.event)
			}
			continue
		}
		switch req.Op {
		case "presence.watch":
			page, stop, err := s.watchPresence(ctx, p, actor, req.Data)
			if err != nil {
				p.send(s.commands.Failed(req.ID, err))
				continue
			}
			if stopPresence != nil {
				stopPresence()
			}
			stopPresence = stop
			body, _ := json.Marshal(page)
			p.send(rpc.Response{ID: req.ID, Data: body})
			continue
		case "events.confirm":
			ack, err := s.confirmEvents(ctx, actor, confirmed, req.Data)
			if err != nil {
				p.send(s.commands.Failed(req.ID, err))
				continue
			}
			confirmed = ack.Through
			body, _ := json.Marshal(ack)
			p.send(rpc.Response{ID: req.ID, Data: body})
			continue
		}
		response := s.commands.Execute(ctx, actor, clientIP(r), req)
		if req.Op == "account.logout" && response.Error == nil {
			s.writeImmediate(ctx, socket, response)
			return
		}
		p.send(response)
	}
}

type identityData struct {
	User  int64 `json:"user_id,omitempty"`
	Chat  bool  `json:"chat_enabled"`
	Admin bool  `json:"site_admin"`
	Head  int64 `json:"head,string"`
	Floor int64 `json:"retained_through,string"`
}

func (s *Server) identityData(ctx context.Context, actor rpc.Actor) (identityData, error) {
	id := identityData{User: actor.ID, Chat: actor.Chat, Admin: actor.Admin}
	if actor.Chat {
		check, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		head, floor, err := s.hub.Head(check)
		if err != nil {
			return id, err
		}
		id.Head, id.Floor = head, floor
	}
	return id, nil
}

func (s *Server) hello(ctx context.Context, p *peer, actor rpc.Actor) bool {
	info, err := s.identityData(ctx, actor)
	if err != nil {
		return false
	}
	p.send(struct {
		Type string `json:"type"`
		identityData
		serverClock
		Protocol int `json:"version"`
		MaxBytes int `json:"max_command_bytes"`
	}{"hello", info, currentClock(), 2, rpc.MaxCommandBytes})
	return true
}

func (p *peer) event(e realtime.Event) {
	switch e.Kind {
	case "account.sessions_changed", "account.status_changed", "account.password_changed", "account.email_verified", "presence.changed":
		p.verify.Store(true)
	}
	p.enqueue(e.Wire)
}

func (p *peer) send(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		p.cancel()
		return
	}
	p.enqueue(data)
}

func (s *Server) writePeer(ctx context.Context, socket *netserve.Socket, p *peer) {
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()
	verify := func() bool {
		id := p.identity.Load()
		if id.token == "" {
			return true
		}
		check, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		actor, err := s.auth.Authenticate(check, id.token)
		return err == nil && actor.ID == id.user && actor.Chat == id.chat
	}
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-p.out:
			if p.verify.Swap(false) && !verify() {
				p.releaseFrame(msg)
				_ = socket.Close(1008, "Session changed")
				return
			}
			err := socket.Write(ctx, msg)
			p.releaseFrame(msg)
			if err != nil {
				return
			}
		case <-ticker.C:
			if !verify() {
				_ = socket.Close(1008, "Session changed")
				return
			}
			if err := socket.Ping(ctx, nil); err != nil {
				return
			}
			if id := p.identity.Load().user; id > 0 {
				s.hub.Touch(id)
			}
		}
	}
}

func (s *Server) writeImmediate(ctx context.Context, socket *netserve.Socket, resp rpc.Response) {
	data, err := json.Marshal(resp)
	if err == nil {
		_ = socket.Write(ctx, data)
	}
}

type bucket struct {
	tokens            float64
	last, timeStarted time.Time
}

func (b *bucket) started() time.Time {
	if b.timeStarted.IsZero() {
		b.timeStarted = time.Now()
	}
	return b.timeStarted
}
func (b *bucket) allow(known bool) bool {
	now := time.Now()
	capacity, rate := 120.0, 30.0
	if !known {
		capacity, rate = 12, 0.5
	}
	b.tokens = min(capacity, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

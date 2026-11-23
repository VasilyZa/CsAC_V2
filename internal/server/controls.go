package server

import (
	"context"
	"encoding/json"
	"time"

	"csac/v2/internal/realtime"
	"csac/v2/internal/rpc"
)

type serverClock struct {
	Time time.Time `json:"server_time"`
}

func currentClock() serverClock { return serverClock{Time: time.Now().UTC()} }

func readServerTime(_ context.Context, _ rpc.Call, raw json.RawMessage) (any, error) {
	if _, err := rpc.Decode[struct{}](raw); err != nil {
		return nil, err
	}
	return currentClock(), nil
}

func chatAccess(actor rpc.Actor) error {
	if actor.ID == 0 {
		return rpc.Unauthenticated()
	}
	if !actor.Chat {
		return rpc.Denied("Complete account verification")
	}
	return nil
}

func (s *Server) watchPresence(ctx context.Context, p *peer, actor rpc.Actor, raw json.RawMessage) (realtime.PresencePage, func(), error) {
	var empty realtime.PresencePage
	if err := chatAccess(actor); err != nil {
		return empty, nil, err
	}
	in, err := rpc.Decode[struct {
		UserIDs []int64 `json:"user_ids"`
	}](raw)
	if err != nil {
		return empty, nil, err
	}
	if err := realtime.CheckPresenceIDs(in.UserIDs); err != nil {
		return empty, nil, err
	}
	var stop func()
	if len(in.UserIDs) != 0 {
		// Install interest before taking the initial snapshot so a concurrent
		// transition cannot fall between subscription and the initial read.
		stop = s.hub.WatchPresence(actor.ID, in.UserIDs, p.event)
	}
	check, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	page, err := s.hub.Presence(check, actor.ID, in.UserIDs)
	if err != nil && stop != nil {
		stop()
	}
	return page, stop, err
}

type eventConfirmation struct {
	Through int64 `json:"confirmed_through,string"`
}

func (s *Server) confirmEvents(ctx context.Context, actor rpc.Actor, previous int64, raw json.RawMessage) (eventConfirmation, error) {
	if err := chatAccess(actor); err != nil {
		return eventConfirmation{}, err
	}
	in, err := rpc.Decode[struct {
		Through *int64 `json:"through,string"`
	}](raw)
	if err != nil {
		return eventConfirmation{}, err
	}
	if in.Through == nil || *in.Through < 0 {
		return eventConfirmation{}, rpc.Invalid("A nonnegative event cursor is required")
	}
	if *in.Through > previous {
		check, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		head, _, err := s.hub.Head(check)
		if err != nil {
			return eventConfirmation{}, err
		}
		if *in.Through > head {
			return eventConfirmation{}, rpc.Invalid("Cursor is ahead of the event head")
		}
	}
	return eventConfirmation{Through: max(previous, *in.Through)}, nil
}

package chat

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"csac/v2/internal/realtime"
	"csac/v2/internal/rpc"
)

func (s service) presence(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		conversationRef
		UserIDs []int64 `json:"user_ids"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if err := realtime.CheckPresenceIDs(in.UserIDs); err != nil {
		return nil, err
	}
	if in.ConversationID != 0 {
		if _, err := accessTo(ctx, call.DB, in.ConversationID, call.Actor.ID, false); err != nil {
			return nil, err
		}
		members, err := memberIDs(ctx, call.DB, in.ConversationID)
		if err != nil {
			return nil, err
		}
		in.UserIDs = slices.DeleteFunc(in.UserIDs, func(id int64) bool { return !slices.Contains(members, id) })
	} else if in.UserIDs == nil {
		in.UserIDs = []int64{call.Actor.ID}
	}
	page, err := s.live.Presence(ctx, call.Actor.ID, in.UserIDs)
	if err != nil {
		return nil, err
	}
	online := make([]int64, 0, len(page.Users))
	for _, user := range page.Users {
		if user.Online {
			online = append(online, user.UserID)
		}
	}
	return struct {
		realtime.PresencePage
		OnlineUserIDs []int64 `json:"online_user_ids"`
	}{page, online}, nil
}

func (s service) typing(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		conversationRef
		Active bool `json:"active"`
	}](raw)
	if err != nil {
		return nil, err
	}
	a, err := accessTo(ctx, call.DB, in.ConversationID, call.Actor.ID, true)
	if err != nil {
		return nil, err
	}
	if a.muted {
		return nil, rpc.Denied("You are muted in this group")
	}
	window := s.typers.window(typingKey{user: call.Actor.ID, conversation: in.ConversationID})
	if !window.available(in.Active) {
		return nil, nil
	}
	ids, err := memberIDs(ctx, call.DB, in.ConversationID)
	if err != nil {
		return nil, err
	}
	ids = slices.DeleteFunc(ids, func(uid int64) bool { return uid == call.Actor.ID })
	return nil, window.publish(in.Active, func() error {
		return s.live.Ephemeral("conversation.typing", ids, struct {
			conversationRef
			UserID int64 `json:"user_id"`
			Active bool  `json:"active"`
			TTL    int   `json:"ttl_seconds"`
		}{in.conversationRef, call.Actor.ID, in.Active, int(typingTTL / time.Second)})
	})
}

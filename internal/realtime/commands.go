package realtime

import (
	"context"
	"encoding/json"

	"csac/v2/internal/rpc"
)

func (h *Hub) Register(reg *rpc.Registry) {
	reg.Register("events.head", rpc.Method{Access: rpc.Member, Handle: func(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
		if _, err := rpc.Decode[struct{}](raw); err != nil {
			return nil, err
		}
		head, floor, err := h.Head(ctx)
		return struct {
			Head     int64 `json:"head,string"`
			Retained int64 `json:"retained_through,string"`
		}{head, floor}, err
	}})
	reg.Register("events.replay", rpc.Method{Access: rpc.Member, Handle: func(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
		data, err := rpc.Decode[struct {
			After int64 `json:"after,string"`
			Limit int   `json:"limit"`
		}](raw)
		if err != nil {
			return nil, err
		}
		if data.After < 0 {
			return nil, rpc.Invalid("Cursor must be nonnegative")
		}
		if data.Limit == 0 {
			data.Limit = 100
		}
		return h.Replay(ctx, call.Actor.ID, data.After, data.Limit)
	}})
}

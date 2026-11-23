// Package realtime delivers durable events and coalesces database wake-ups.
package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Event struct {
	Seq       int64           `json:"seq,string,omitempty"`
	Kind      string          `json:"event"`
	Data      json.RawMessage `json:"data"`
	CreatedAt time.Time       `json:"at"`
	Wire      []byte          `json:"-"`
}

type eventRecord struct {
	Event
	Recipients []int64
}

type Subscription func(Event)

type Hub struct {
	pool     *pgxpool.Pool
	log      *slog.Logger
	mu       sync.RWMutex
	next     uint64
	subs     map[int64]map[uint64]Subscription
	wake     chan struct{}
	presence presenceState
}

func New(pool *pgxpool.Pool, logger *slog.Logger) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hub{
		pool: pool, log: logger, subs: make(map[int64]map[uint64]Subscription), wake: make(chan struct{}, 1),
		presence: presenceState{
			pending: make(map[int64]time.Time), changed: make(map[int64]struct{}),
			watchers: make(map[uint64]*presenceWatcher),
		},
	}
}

func (h *Hub) Subscribe(user int64, deliver Subscription) func() {
	h.mu.Lock()
	h.next++
	id := h.next
	if h.subs[user] == nil {
		h.subs[user] = make(map[uint64]Subscription)
		h.presence.record(user, true)
	}
	h.subs[user][id] = deliver
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, subscribed := h.subs[user][id]; !subscribed {
			return
		}
		delete(h.subs[user], id)
		if len(h.subs[user]) == 0 {
			delete(h.subs, user)
			h.presence.record(user, true)
		}
	}
}

func (h *Hub) Online(id int64) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[id]) != 0
}

func (h *Hub) Ephemeral(kind string, to []int64, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	h.deliver(eventRecord{Event: Event{Kind: kind, Data: data, CreatedAt: time.Now().UTC()}, Recipients: to})
	return nil
}

func (h *Hub) deliverTo(kind string, value any, send Subscription) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	event := Event{Kind: kind, Data: data, CreatedAt: time.Now().UTC()}
	if !event.makeFrame() {
		return nil
	}
	send(event)
	return nil
}

func (e *Event) makeFrame() bool {
	wire, err := json.Marshal(struct {
		Type string `json:"type"`
		Event
	}{"event", *e})
	if err != nil {
		return false
	}
	e.Wire = wire
	return true
}

func (h *Hub) deliver(record eventRecord) {
	if len(record.Recipients) == 0 {
		return
	}
	if !record.Event.makeFrame() {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, uid := range record.Recipients {
		for _, send := range h.subs[uid] {
			send(record.Event)
		}
	}
}

func (h *Hub) Head(ctx context.Context) (head, retained int64, err error) {
	err = h.pool.QueryRow(ctx, `SELECT head,retained_through FROM event_clock WHERE singleton`).Scan(&head, &retained)
	return
}

// Start reads the initial watermark before accepting socket connections.
func (h *Hub) Start(ctx context.Context, retention time.Duration) (<-chan struct{}, error) {
	head, _, err := h.Head(ctx)
	if err != nil {
		return nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		var workers sync.WaitGroup
		workers.Go(func() { h.listen(ctx) })
		workers.Go(func() { h.runPresence(ctx) })
		h.loop(ctx, head, retention)
		workers.Wait()
	}()
	return done, nil
}

func (h *Hub) listen(ctx context.Context) {
	config := h.pool.Config().ConnConfig
	for ctx.Err() == nil {
		conn, err := pgx.ConnectConfig(ctx, config.Copy())
		if err == nil {
			_, err = conn.Exec(ctx, `LISTEN csac_events`)
			for err == nil {
				h.signal()
				_, err = conn.WaitForNotification(ctx)
			}
			closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = conn.Close(closeCtx)
			cancel()
		}
		if ctx.Err() != nil {
			return
		}
		h.log.Warn("event wake-up channel disconnected; polling continues")
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (h *Hub) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *Hub) loop(ctx context.Context, head int64, retention time.Duration) {
	poll := time.NewTicker(250 * time.Millisecond)
	purge := time.NewTicker(time.Minute)
	defer poll.Stop()
	defer purge.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-purge.C:
			if err := h.prune(ctx, retention); err != nil && ctx.Err() == nil {
				h.log.Warn("event pruning failed")
			}
			continue
		case <-poll.C:
		case <-h.wake:
		}
		for ctx.Err() == nil {
			records, err := h.load(ctx, head)
			if err != nil {
				if ctx.Err() == nil {
					h.log.Warn("event read failed")
				}
				break
			}
			for _, record := range records {
				h.deliver(record)
				head = record.Seq
			}
			if len(records) < 256 {
				break
			}
		}
	}
}

func (h *Hub) load(ctx context.Context, after int64) ([]eventRecord, error) {
	rows, err := h.pool.Query(ctx, `WITH page AS MATERIALIZED (
		SELECT seq,kind,recipients,data,created_at,conversation_id,message_seq FROM events WHERE seq>$1 ORDER BY seq LIMIT 256)
		SELECT e.seq,e.kind,CASE
			WHEN e.conversation_id IS NULL THEN e.recipients
			WHEN e.message_seq IS NOT NULL AND NOT EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=e.conversation_id AND m.seq=e.message_seq AND m.recalled_at IS NULL) THEN '{}'::bigint[]
			ELSE ARRAY(SELECT user_id FROM chat_readers r WHERE r.conversation_id=e.conversation_id AND r.user_id=ANY(e.recipients))
		END,e.data,e.created_at FROM page e ORDER BY e.seq`, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []eventRecord
	for rows.Next() {
		var r eventRecord
		if err := rows.Scan(&r.Seq, &r.Kind, &r.Recipients, &r.Data, &r.CreatedAt); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

type Replay struct {
	Events          []Event `json:"events"`
	Next            int64   `json:"next,string"`
	Head            int64   `json:"head,string"`
	RetainedThrough int64   `json:"retained_through,string"`
	HasMore         bool    `json:"has_more"`
	Reset           bool    `json:"reset"`
}

func (h *Hub) Replay(ctx context.Context, uid, after int64, limit int) (page Replay, err error) {
	page.Events = make([]Event, 0)
	limit = min(100, max(1, limit))
	err = pgx.BeginTxFunc(ctx, h.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT head,retained_through FROM event_clock WHERE singleton`).Scan(&page.Head, &page.RetainedThrough); err != nil {
			return err
		}
		page.Next = page.Head
		if after < page.RetainedThrough || after > page.Head {
			page.Reset = true
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT e.seq,e.kind,e.data,e.created_at FROM events e
			WHERE e.seq>$1 AND e.seq<=$2 AND e.recipients @> ARRAY[$3]::bigint[]
			AND (e.conversation_id IS NULL OR EXISTS(SELECT 1 FROM chat_readers r WHERE r.conversation_id=e.conversation_id AND r.user_id=$3))
			AND (e.message_seq IS NULL OR EXISTS(SELECT 1 FROM messages m WHERE m.conversation_id=e.conversation_id AND m.seq=e.message_seq AND m.recalled_at IS NULL))
			ORDER BY e.seq LIMIT $4`, after, page.Head, uid, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		size := 0
		for rows.Next() {
			var e Event
			if err := rows.Scan(&e.Seq, &e.Kind, &e.Data, &e.CreatedAt); err != nil {
				return err
			}
			size += len(e.Data) + 128
			if len(page.Events) == limit || size > 384<<10 {
				page.HasMore = true
				page.Next = page.Events[len(page.Events)-1].Seq
				break
			}
			page.Events = append(page.Events, e)
		}
		return rows.Err()
	})
	return
}

func (h *Hub) prune(ctx context.Context, retention time.Duration) error {
	cutoff := time.Now().UTC().Add(-retention)
	return pgx.BeginFunc(ctx, h.pool, func(tx pgx.Tx) error {
		// Limit each deletion and update the floor in the same transaction.
		var currentFloor int64
		if err := tx.QueryRow(ctx, `SELECT retained_through FROM event_clock WHERE singleton FOR UPDATE`).Scan(&currentFloor); err != nil {
			return err
		}
		var through *int64
		if err := tx.QueryRow(ctx, `SELECT max(seq) FROM (SELECT seq FROM events WHERE seq>$1 AND created_at<$2 ORDER BY seq LIMIT 20000) expired`, currentFloor, cutoff).Scan(&through); err != nil {
			return err
		}
		if through != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM events WHERE seq<=$1`, *through); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE event_clock SET retained_through=$1 WHERE singleton`, *through); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM command_keys k USING (SELECT user_id,key FROM command_keys WHERE created_at<$1 LIMIT 20000 FOR UPDATE SKIP LOCKED) expired WHERE k.user_id=expired.user_id AND k.key=expired.key`, cutoff)
		return err
	})
}

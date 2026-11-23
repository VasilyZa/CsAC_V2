package realtime

import (
	"context"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"csac/v2/internal/rpc"
)

const MaxPresenceUsers = 200

const (
	presenceUpdateInterval = 100 * time.Millisecond
	activityFlushInterval  = 30 * time.Second
)

type PresenceEntry struct {
	UserID     int64      `json:"user_id"`
	Online     bool       `json:"online"`
	LastActive *time.Time `json:"last_active"`
}

type PresencePage struct {
	Users []PresenceEntry `json:"users"`
}

type presenceState struct {
	mu       sync.Mutex
	pending  map[int64]time.Time
	changed  map[int64]struct{}
	next     uint64
	watchers map[uint64]*presenceWatcher
}

type presenceWatcher struct {
	viewer  int64
	users   []int64
	send    Subscription
	stopped atomic.Bool
}

// Touch records authenticated activity without a database write per request.
func (h *Hub) Touch(user int64) {
	if user <= 0 {
		return
	}
	h.presence.record(user, false)
}

func (s *presenceState) record(user int64, connectionChanged bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending[user] = time.Now().UTC()
	if connectionChanged {
		s.changed[user] = struct{}{}
	}
}

// CheckPresenceIDs bounds both a snapshot and the per-connection interest set.
func CheckPresenceIDs(users []int64) error {
	if len(users) > MaxPresenceUsers {
		return rpc.Invalid("At most 200 presence checks per request")
	}
	for _, id := range users {
		if id <= 0 {
			return rpc.Invalid("Presence user IDs must be positive")
		}
	}
	return nil
}

func (h *Hub) Presence(ctx context.Context, viewer int64, userIDs []int64) (PresencePage, error) {
	page := PresencePage{Users: make([]PresenceEntry, 0, len(userIDs))}
	if err := CheckPresenceIDs(userIDs); err != nil {
		return page, err
	}
	pairs := make([]presencePair, 0, len(userIDs))
	seen := make(map[int64]struct{}, len(userIDs))
	for _, id := range userIDs {
		if _, exists := seen[id]; !exists {
			pairs = append(pairs, presencePair{viewer, id})
			seen[id] = struct{}{}
		}
	}
	allowed, err := h.readPresence(ctx, pairs)
	if err != nil {
		return page, err
	}
	for _, pair := range pairs {
		if last, ok := allowed[pair]; ok {
			page.Users = append(page.Users, h.presenceEntry(pair.subject, last))
		}
	}
	return page, nil
}

func (h *Hub) WatchPresence(viewer int64, userIDs []int64, send Subscription) func() {
	users := slices.Clone(userIDs)
	slices.Sort(users)
	watch := &presenceWatcher{viewer: viewer, users: slices.Compact(users), send: send}
	h.presence.mu.Lock()
	h.presence.next++
	id := h.presence.next
	h.presence.watchers[id] = watch
	h.presence.mu.Unlock()
	return func() {
		watch.stopped.Store(true)
		h.presence.mu.Lock()
		delete(h.presence.watchers, id)
		h.presence.mu.Unlock()
	}
}

type presencePair struct{ viewer, subject int64 }

// Both reads and push notifications use the current relationship and block rules.
func (h *Hub) readPresence(ctx context.Context, pairs []presencePair) (map[presencePair]*time.Time, error) {
	out := make(map[presencePair]*time.Time, len(pairs))
	if len(pairs) == 0 {
		return out, nil
	}
	viewers, subjects := make([]int64, len(pairs)), make([]int64, len(pairs))
	for i, p := range pairs {
		viewers[i], subjects[i] = p.viewer, p.subject
	}
	rows, err := h.pool.Query(ctx, `SELECT v.viewer,v.subject,l.last_active
		FROM unnest($1::bigint[], $2::bigint[]) AS v(viewer,subject)
		JOIN accounts subject ON subject.id=v.subject
		JOIN accounts viewer ON viewer.id=v.viewer
		LEFT JOIN user_activity l ON l.user_id=v.subject
		WHERE (subject.status='active' OR (subject.status='banned' AND subject.ban_until<=statement_timestamp()))
		AND (viewer.status='active' OR (viewer.status='banned' AND viewer.ban_until<=statement_timestamp()))
		AND (v.viewer=v.subject OR (
			NOT EXISTS (SELECT 1 FROM friend_preferences b WHERE b.owner_id=v.viewer AND b.other_id=v.subject AND b.blocked)
			AND NOT EXISTS (SELECT 1 FROM friend_preferences b WHERE b.owner_id=v.subject AND b.other_id=v.viewer AND b.blocked)
			AND (EXISTS (SELECT 1 FROM friendships f WHERE f.user_low=least(v.viewer,v.subject) AND f.user_high=greatest(v.viewer,v.subject))
			OR EXISTS (SELECT 1 FROM conversation_members m
				JOIN conversations c ON c.id=m.conversation_id
				JOIN conversation_members s ON s.conversation_id=m.conversation_id AND s.user_id=v.subject
				WHERE m.user_id=v.viewer AND c.kind='group' AND c.archived_at IS NULL
				AND (c.site_ban_until IS NULL OR c.site_ban_until<=statement_timestamp())))))`, viewers, subjects)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pair presencePair
		var last *time.Time
		if err := rows.Scan(&pair.viewer, &pair.subject, &last); err != nil {
			return nil, err
		}
		out[pair] = last
	}
	return out, rows.Err()
}

func (h *Hub) presenceEntry(user int64, stored *time.Time) PresenceEntry {
	h.presence.mu.Lock()
	latest, ok := h.presence.pending[user]
	h.presence.mu.Unlock()
	if ok && (stored == nil || latest.After(*stored)) {
		stored = &latest
	}
	if stored != nil {
		utc := stored.UTC()
		stored = &utc
	}
	return PresenceEntry{UserID: user, Online: h.Online(user), LastActive: stored}
}

func (h *Hub) runPresence(ctx context.Context) {
	updates := time.NewTicker(presenceUpdateInterval)
	activity := time.NewTicker(activityFlushInterval)
	defer updates.Stop()
	defer activity.Stop()
	for {
		select {
		case <-ctx.Done():
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			if err := h.flushActivity(final); err != nil {
				h.log.Warn("activity flush on shutdown failed")
			}
			cancel()
			return
		case <-activity.C:
			work, cancel := context.WithTimeout(ctx, 3*time.Second)
			if err := h.flushActivity(work); err != nil && ctx.Err() == nil {
				h.log.Warn("activity flush failed")
			}
			cancel()
		case <-updates.C:
			h.tickPresence(ctx)
		}
	}
}

func (h *Hub) tickPresence(ctx context.Context) {
	h.presence.mu.Lock()
	if len(h.presence.changed) == 0 {
		h.presence.mu.Unlock()
		return
	}
	changed := h.presence.changed
	h.presence.changed = make(map[int64]struct{})
	watchers := slices.Collect(maps.Values(h.presence.watchers))
	h.presence.mu.Unlock()

	work, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := h.flushActivity(work); err != nil {
		if ctx.Err() == nil {
			h.log.Warn("presence activity flush failed")
		}
		return
	}
	for batch := range slices.Chunk(watchers, 32) {
		if err := h.notifyPresence(work, changed, batch); err != nil {
			if ctx.Err() == nil {
				h.log.Warn("presence notification failed")
			}
			return
		}
	}
}

func (h *Hub) flushActivity(ctx context.Context) error {
	h.presence.mu.Lock()
	pending := maps.Clone(h.presence.pending)
	h.presence.mu.Unlock()
	if len(pending) == 0 {
		return nil
	}
	users := slices.Sorted(maps.Keys(pending))
	values := make([]time.Time, len(users))
	for i, user := range users {
		values[i] = pending[user]
	}
	_, err := h.pool.Exec(ctx, `INSERT INTO user_activity(user_id,last_active)
		SELECT display.user_id,display.last_active FROM unnest($1::bigint[], $2::timestamptz[]) AS display(user_id,last_active)
		ORDER BY display.user_id ON CONFLICT (user_id) DO UPDATE SET last_active=excluded.last_active
		WHERE user_activity.last_active<excluded.last_active`, users, values)
	if err != nil {
		return err
	}
	h.presence.mu.Lock()
	defer h.presence.mu.Unlock()
	for user, at := range pending {
		if h.presence.pending[user] == at {
			delete(h.presence.pending, user)
		}
	}
	return nil
}

func (h *Hub) notifyPresence(ctx context.Context, changed map[int64]struct{}, watchers []*presenceWatcher) error {
	var pairs []presencePair
	interest := make([][]presencePair, len(watchers))
	for i, watch := range watchers {
		if watch.stopped.Load() {
			continue
		}
		for _, user := range watch.users {
			if _, exists := changed[user]; exists {
				pair := presencePair{watch.viewer, user}
				interest[i] = append(interest[i], pair)
				pairs = append(pairs, pair)
			}
		}
	}
	allowed, err := h.readPresence(ctx, pairs)
	if err != nil {
		return err
	}
	for i, watching := range interest {
		watch := watchers[i]
		if len(watching) == 0 || watch.stopped.Load() {
			continue
		}
		var page PresencePage
		for _, pair := range watching {
			if last, ok := allowed[pair]; ok {
				page.Users = append(page.Users, h.presenceEntry(pair.subject, last))
			}
		}
		if len(page.Users) != 0 {
			if err := h.deliverTo("presence.changed", page, watch.send); err != nil {
				return err
			}
		}
	}
	return nil
}

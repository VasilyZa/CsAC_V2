package chat

import (
	"hash/maphash"
	"sync"
	"time"
)

const (
	typingInterval      = time.Second
	typingTTL           = 5 * time.Second
	typingShardCapacity = 1024
)

type typingKey struct{ user, conversation int64 }

type typingEntry struct {
	lastStart time.Time
	active    bool
}

type typingShard struct {
	mu        sync.Mutex
	nextSweep time.Time
	entries   map[typingKey]typingEntry
}

type typingWindows struct {
	seed   maphash.Seed
	shards [32]typingShard
}

func newTypingWindows() *typingWindows {
	return &typingWindows{seed: maphash.MakeSeed()}
}

type typingWindow struct {
	shard *typingShard
	key   typingKey
}

func (w *typingWindows) window(key typingKey) typingWindow {
	return typingWindow{&w.shards[maphash.Comparable(w.seed, key)%uint64(len(w.shards))], key}
}

func (s *typingShard) sweep(now time.Time) {
	if now.Before(s.nextSweep) {
		return
	}
	for key, entry := range s.entries {
		if now.Sub(entry.lastStart) >= typingTTL {
			delete(s.entries, key)
		}
	}
	s.nextSweep = now.Add(typingInterval)
}

func (w typingWindow) allowed(active bool, now time.Time) bool {
	entry, tracked := w.shard.entries[w.key]
	if !active {
		// An effective stop is never subject to the start rate limit. An
		// unannounced or already expired start has nothing to retract.
		return entry.active && now.Sub(entry.lastStart) < typingTTL
	}
	return (!tracked && len(w.shard.entries) < typingShardCapacity) ||
		(tracked && now.Sub(entry.lastStart) >= typingInterval)
}

func (w typingWindow) available(active bool) bool {
	w.shard.mu.Lock()
	defer w.shard.mu.Unlock()
	now := time.Now()
	w.shard.sweep(now)
	return w.allowed(active, now)
}

func (w typingWindow) publish(active bool, send func() error) error {
	// Recheck after the membership lookup. Serialize state changes with the
	// in-memory enqueue so starts and stops from different sockets cannot swap.
	w.shard.mu.Lock()
	defer w.shard.mu.Unlock()
	now := time.Now()
	w.shard.sweep(now)
	if !w.allowed(active, now) {
		return nil
	}
	if err := send(); err != nil {
		return err
	}
	if w.shard.entries == nil {
		w.shard.entries = make(map[typingKey]typingEntry)
	}
	entry := w.shard.entries[w.key]
	if active {
		entry.lastStart = now
	}
	entry.active = active
	w.shard.entries[w.key] = entry
	return nil
}

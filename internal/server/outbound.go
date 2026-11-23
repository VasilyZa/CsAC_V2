package server

import (
	"context"
	"sync/atomic"
)

const (
	peerQueueBytes   = 2 << 20
	globalQueueBytes = 32 << 20
)

// Counts logical bytes pending per socket, including the write in progress.
// Broadcasts may share the payload slice across multiple sockets.
type outboundBudget struct{ pending atomic.Int64 }

func (b *outboundBudget) reserve(n int64) bool {
	for current := b.pending.Load(); n <= globalQueueBytes-current; current = b.pending.Load() {
		if b.pending.CompareAndSwap(current, current+n) {
			return true
		}
	}
	return false
}

func (s *Server) newPeer(cancel context.CancelFunc) *peer {
	return &peer{cancel: cancel, out: make(chan []byte, 64), budget: &s.outbound}
}

func (p *peer) enqueue(data []byte) {
	p.queueMu.Lock()
	if p.closed || p.overloaded.Load() {
		p.queueMu.Unlock()
		return
	}
	n := int64(len(data))
	if len(p.out) < cap(p.out) && n <= peerQueueBytes-p.queued.Load() && p.budget.reserve(n) {
		p.queued.Add(n)
		// Only the writer can remove frames; producers are serialized by queueMu.
		p.out <- data
		p.queueMu.Unlock()
		return
	}
	p.overloaded.Store(true)
	p.queueMu.Unlock()
	p.cancel()
}

func (p *peer) releaseFrame(frame []byte) {
	n := int64(len(frame))
	p.queued.Add(-n)
	p.budget.pending.Add(-n)
}

// The writer has stopped, so all outstanding reservations are now in out.
func (p *peer) discardQueue() {
	p.queueMu.Lock()
	defer p.queueMu.Unlock()
	p.closed = true
	for len(p.out) != 0 {
		p.releaseFrame(<-p.out)
	}
}

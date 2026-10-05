package loghub

import (
	"sync"
	"sync/atomic"

	"github.com/tuvo1106/ozymandias/internal/query/logql"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// DefaultBuffer is a subscriber's channel capacity.
const DefaultBuffer = 1000

// MaxSubscribers bounds concurrent tails. Each costs a goroutine's worth of
// work per published batch, and a few tabs is the intended use.
const MaxSubscribers = 64

// Hub delivers published logs to the subscribers whose filter matches.
type Hub struct {
	mu   sync.RWMutex
	subs map[*Subscription]struct{}
	max  int

	published, delivered, dropped atomic.Int64
}

// New returns a hub allowing at most max subscribers (zero: MaxSubscribers).
func New(max int) *Hub {
	if max <= 0 {
		max = MaxSubscribers
	}
	return &Hub{subs: map[*Subscription]struct{}{}, max: max}
}

// Subscription is one tail. Read C; call Close when done.
type Subscription struct {
	// C carries matching logs. It is closed by Close, never by the hub.
	C       chan wire.Log
	filter  logql.Filter
	hub     *Hub
	dropped atomic.Int64
	taken   atomic.Int64 // dropped count already reported by TakeDropped
}

// Subscribe registers a tail with filter (nil matches every log). It returns
// false when the hub is full.
func (h *Hub) Subscribe(filter logql.Node, buffer int) (*Subscription, bool) {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	s := &Subscription{C: make(chan wire.Log, buffer), filter: logql.Compile(filter), hub: h}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.subs) >= h.max {
		return nil, false
	}
	h.subs[s] = struct{}{}
	return s, true
}

// Close unregisters the subscription. It is safe to call more than once. The
// channel is left open: publishers may still hold a reference for an instant,
// and a send on a closed channel would panic.
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	delete(s.hub.subs, s)
	s.hub.mu.Unlock()
}

// TakeDropped returns how many logs were dropped for this subscriber since the
// last call, for the "dropped" notice.
func (s *Subscription) TakeDropped() int64 {
	total := s.dropped.Load()
	prev := s.taken.Swap(total)
	return total - prev
}

// Publish offers logs to every subscriber. It never blocks: a full channel
// drops the log for that subscriber.
func (h *Hub) Publish(logs []wire.Log) {
	h.published.Add(int64(len(logs)))
	h.mu.RLock()
	defer h.mu.RUnlock()
	if len(h.subs) == 0 {
		return
	}
	for i := range logs {
		l := &logs[i]
		for s := range h.subs {
			if !s.filter(l) {
				continue
			}
			select {
			case s.C <- *l:
				h.delivered.Add(1)
			default:
				s.dropped.Add(1)
				h.dropped.Add(1)
			}
		}
	}
}

// Stats are the hub's counters.
type Stats struct {
	Subscribers                   int
	Published, Delivered, Dropped int64
}

// Stats returns the counters so far.
func (h *Hub) Stats() Stats {
	h.mu.RLock()
	n := len(h.subs)
	h.mu.RUnlock()
	return Stats{n, h.published.Load(), h.delivered.Load(), h.dropped.Load()}
}

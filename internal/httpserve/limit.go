package httpserve

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
)

// Limiter bounds how many requests of one kind run at once.
//
// The reason is isolation: a dashboard with thirty widgets, or a runaway
// script, can start more queries than the machine has cores, and each one
// competes with ingest for CPU, disk and the TSDB's locks. A fixed number of
// slots keeps query load a bounded fraction of the machine; a short queue
// absorbs the burst a page load makes; past the queue the answer is an
// immediate 503, because a request that waits for ever is a client that
// retries on top of itself.
type Limiter struct {
	slots   chan struct{}
	waiting atomic.Int64
	// MaxQueue is how many requests may wait for a slot at once.
	maxQueue int64
	// wait is how long one may wait.
	wait  time.Duration
	clock clock.Clock

	rejected atomic.Int64
}

// NewLimiter returns a limiter with max slots, room for queue waiters, each
// waiting at most wait. max must be positive.
func NewLimiter(clk clock.Clock, max, queue int, wait time.Duration) *Limiter {
	return &Limiter{slots: make(chan struct{}, max), maxQueue: int64(queue), wait: wait, clock: clk}
}

// InFlight is the number of requests holding a slot.
func (l *Limiter) InFlight() int { return len(l.slots) }

// Queued is the number of requests waiting for one.
func (l *Limiter) Queued() int { return int(l.waiting.Load()) }

// Rejected counts requests answered 503.
func (l *Limiter) Rejected() int64 { return l.rejected.Load() }

// Wrap applies the limit to the requests applies selects and passes the rest
// through untouched. Long-lived streams (live tail) must not be selected: one
// would hold a slot for as long as the page is open.
func (l *Limiter) Wrap(h http.Handler, applies func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !applies(r) {
			h.ServeHTTP(w, r)
			return
		}
		if !l.acquire(r) {
			l.rejected.Add(1)
			w.Header().Set("Retry-After", strconv.Itoa(1))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"status":"error","error":"too many concurrent queries; retry shortly"}` + "\n"))
			return
		}
		defer func() { <-l.slots }()
		h.ServeHTTP(w, r)
	})
}

func (l *Limiter) acquire(r *http.Request) bool {
	select {
	case l.slots <- struct{}{}:
		return true
	default:
	}
	if l.waiting.Add(1) > l.maxQueue {
		l.waiting.Add(-1)
		return false
	}
	defer l.waiting.Add(-1)
	t := l.clock.NewTimer(l.wait)
	defer t.Stop()
	select {
	case l.slots <- struct{}{}:
		return true
	case <-t.C():
		return false
	case <-r.Context().Done():
		return false
	}
}

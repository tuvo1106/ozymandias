package testutil

import (
	"sort"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
)

// FakeClock is a [clock.Clock] whose time only moves when the test calls
// [FakeClock.Advance] or [FakeClock.Set].
//
// Advancing fires every ticker and timer whose deadline is crossed, in
// deadline order, and moves Now to each deadline as it fires — so a component
// that reads Now inside its tick handler sees the tick's time, as it would in
// production. Delivery matches package time: channels hold one value and a
// tick that finds the channel full is dropped, never queued. A ticker left
// several periods behind by one big Advance therefore delivers its first
// missed deadline and nothing else, and the cost of the Advance does not grow
// with the span skipped.
//
// A fired value only says the channel was written; the goroutine reading it
// runs whenever the scheduler gets to it. Tests therefore assert on the
// component's observable effect with [Eventually], or synchronise through the
// component's own API — never by sleeping. Stop and Reset discard an unread
// fire, matching package time's Go 1.23+ semantics.
//
// The usual race to avoid: calling Advance before the code under test has
// created its ticker. Wait for it first with
// `Eventually(t, time.Second, func() bool { return fc.Waiters() >= 1 })`.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*fakeWaiter
}

var _ clock.Clock = (*FakeClock)(nil)

// NewFakeClock returns a FakeClock reading start.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

// Now returns the fake current time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTicker returns a ticker driven by this clock. Like time.NewTicker it
// panics if d is not positive: that is a bug in the caller, and failing loudly
// in the test beats a ticker that silently never fires.
func (c *FakeClock) NewTicker(d time.Duration) clock.Ticker {
	if d <= 0 {
		panic("testutil: non-positive interval for FakeClock.NewTicker")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &fakeWaiter{clock: c, when: c.now.Add(d), period: d, ch: make(chan time.Time, 1)}
	c.waiters = append(c.waiters, w)
	return tickerView{w}
}

// NewTimer returns a one-shot timer driven by this clock. A non-positive d
// fires on the next Advance (even Advance(0)), matching time.NewTimer's
// "fires immediately".
func (c *FakeClock) NewTimer(d time.Duration) clock.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &fakeWaiter{clock: c, when: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.waiters = append(c.waiters, w)
	return w
}

// Waiters reports how many tickers and timers are currently armed.
func (c *FakeClock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// Advance moves the clock forward by d, firing everything that comes due.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	c.Set(target)
}

// Set moves the clock to t, firing everything due at or before t. Moving
// backwards only changes Now: nothing un-fires, and armed deadlines stay put,
// the same as a wall-clock step on a real host.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		next := c.nextDueLocked(t)
		if next == nil {
			break
		}
		if next.when.After(c.now) {
			c.now = next.when
		}
		next.fireLocked(next.when)
		// fireLocked re-armed a ticker one period on. If that is still at or
		// before t the ticker is behind, and every remaining fire would land
		// in a channel already holding a value and be dropped — see
		// TestFakeClock_TickerDropsTicksWhenReceiverIsBehind. Skip straight to
		// the first deadline after t rather than looping once per period: the
		// channel ends up holding the same value either way, and the loop was
		// O(elapsed/period). Advancing 10,000 hours against a 6s ticker was
		// six million iterations, each re-sorting the waiters — one test spent
		// four minutes there, 83% of its package's runtime.
		if next.period > 0 && !next.when.After(t) {
			skip := t.Sub(next.when)/next.period + 1
			next.when = next.when.Add(skip * next.period)
		}
	}
	c.now = t
}

// nextDueLocked returns the earliest armed waiter due at or before t.
func (c *FakeClock) nextDueLocked(t time.Time) *fakeWaiter {
	sort.SliceStable(c.waiters, func(i, j int) bool { return c.waiters[i].when.Before(c.waiters[j].when) })
	if len(c.waiters) == 0 || c.waiters[0].when.After(t) {
		return nil
	}
	return c.waiters[0]
}

func (c *FakeClock) removeLocked(w *fakeWaiter) bool {
	for i, x := range c.waiters {
		if x == w {
			c.waiters = append(c.waiters[:i], c.waiters[i+1:]...)
			return true
		}
	}
	return false
}

// fakeWaiter is both the fake Ticker (period > 0) and the fake Timer.
type fakeWaiter struct {
	clock  *FakeClock
	when   time.Time
	period time.Duration
	ch     chan time.Time
}

func (w *fakeWaiter) C() <-chan time.Time { return w.ch }

// fireLocked delivers one value and re-arms (ticker) or disarms (timer).
func (w *fakeWaiter) fireLocked(at time.Time) {
	select {
	case w.ch <- at:
	default: // receiver is behind: drop, like package time
	}
	if w.period > 0 {
		w.when = w.when.Add(w.period)
		return
	}
	w.clock.removeLocked(w)
}

// Stop disarms the waiter. For timers it reports whether it was still armed;
// the ticker form ignores the result. Like package time since Go 1.23, a
// value that fired but was never read is discarded: nothing from before Stop
// is received after it. Such a timer also still counts as armed, so Stop
// returns true. Returning false would send `if !t.Stop() { <-t.C }` into a
// receive on the channel just drained.
func (w *fakeWaiter) Stop() bool {
	w.clock.mu.Lock()
	defer w.clock.mu.Unlock()
	drained := w.drainLocked()
	return w.clock.removeLocked(w) || drained
}

// drainLocked discards an unread fire, if any, and reports whether there was one.
func (w *fakeWaiter) drainLocked() bool {
	select {
	case <-w.ch:
		return true
	default:
		return false
	}
}

// Reset re-arms the waiter to fire d from now (and, for tickers, every d
// after), discarding any unread fire as Stop does. It reports whether the
// waiter had been armed, counting a fired-but-unread timer as armed (see Stop).
func (w *fakeWaiter) Reset(d time.Duration) bool {
	w.clock.mu.Lock()
	defer w.clock.mu.Unlock()
	drained := w.drainLocked()
	wasArmed := w.clock.removeLocked(w) || drained
	if w.period > 0 {
		w.period = d
	}
	w.when = w.clock.now.Add(d)
	w.clock.waiters = append(w.clock.waiters, w)
	return wasArmed
}

// tickerView adapts fakeWaiter to clock.Ticker, whose Stop and Reset return
// nothing. Go does not let one type satisfy both signatures, so the ticker
// hands out this wrapper.
type tickerView struct{ w *fakeWaiter }

func (t tickerView) C() <-chan time.Time   { return t.w.ch }
func (t tickerView) Stop()                 { t.w.Stop() }
func (t tickerView) Reset(d time.Duration) { t.w.Reset(d) }

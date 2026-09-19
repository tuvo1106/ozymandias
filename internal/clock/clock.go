package clock

import "time"

// Clock tells the time and creates tickers and timers. Implementations must be
// safe for concurrent use.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// NewTicker returns a ticker that delivers the time on its channel every d.
	// Like time.NewTicker, ticks are dropped (not queued) if the receiver falls
	// behind, and d must be positive.
	NewTicker(d time.Duration) Ticker
	// NewTimer returns a timer that delivers the time on its channel once,
	// after d.
	NewTimer(d time.Duration) Timer
}

// Ticker is the interface form of *time.Ticker, so a fake can stand in for it.
type Ticker interface {
	// C returns the channel ticks are delivered on.
	C() <-chan time.Time
	// Stop turns the ticker off. It does not close the channel.
	Stop()
	// Reset stops the ticker and restarts it with period d.
	Reset(d time.Duration)
}

// Timer is the interface form of *time.Timer, so a fake can stand in for it.
type Timer interface {
	// C returns the channel the single firing is delivered on.
	C() <-chan time.Time
	// Stop prevents the timer from firing. It reports whether the call stopped
	// the timer, false if it had already fired or been stopped.
	Stop() bool
	// Reset changes the timer to fire after d. It reports whether the timer
	// had been active.
	Reset(d time.Duration) bool
}

// Real returns the Clock backed by package time.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTicker(d time.Duration) Ticker { return realTicker{time.NewTicker(d)} }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTicker struct{ t *time.Ticker }

func (r realTicker) C() <-chan time.Time   { return r.t.C }
func (r realTicker) Stop()                 { r.t.Stop() }
func (r realTicker) Reset(d time.Duration) { r.t.Reset(d) }

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time        { return r.t.C }
func (r realTimer) Stop() bool                 { return r.t.Stop() }
func (r realTimer) Reset(d time.Duration) bool { return r.t.Reset(d) }

package logpipeline

import (
	"sync"
	"time"
)

// limiter is a token bucket: up to `rate` lines per second, with a burst of one
// second's worth. It exists so that one runaway logger (a loop printing a
// stack trace) cannot fill the intake and the store; the excess is dropped and
// counted, never queued, because a backlog of noise delays the lines that
// matter.
type limiter struct {
	mu     sync.Mutex
	rate   float64 // tokens per second; <= 0 means unlimited
	tokens float64
	last   time.Time
}

func newLimiter(rate int, now time.Time) *limiter {
	return &limiter{rate: float64(rate), tokens: float64(rate), last: now}
}

// allow reports whether a line may pass at time now.
func (l *limiter) allow(now time.Time) bool {
	if l.rate <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if elapsed := now.Sub(l.last).Seconds(); elapsed > 0 {
		l.tokens = min(l.rate, l.tokens+elapsed*l.rate)
		l.last = now
	}
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

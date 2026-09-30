package collector

import (
	"math"
	"time"
)

// Rates turns successive readings of cumulative counters (bytes received
// since boot, CPU seconds since the container started) into per-second
// rates. One Rates belongs to one collector and is not safe for concurrent
// use, which the Collector contract makes unnecessary.
//
// Three things make this more than a subtraction:
//
//   - The first reading of a key has nothing to subtract from, so it yields
//     no rate. Reporting 0 instead would draw a false dip every time the
//     agent restarts or a new interface appears.
//   - A counter that goes down was reset: the machine rebooted, the container
//     restarted, a 32-bit counter wrapped. The difference is meaningless (it
//     would be a huge negative rate) and the true increase since the reset is
//     unknowable — the reading itself is a lower bound on it, but only if the
//     reset happened right after the previous reading. Prometheus's rate()
//     assumes exactly that and adds the new value; here the reading is
//     skipped instead, costing one interval's rate after a reset and never
//     inventing a spike. The next reading proceeds normally.
//   - Keys disappear (an unplugged interface, a stopped container). [Rates.Prune]
//     forgets them, so memory is bounded by what currently exists rather
//     than by everything that ever did.
type Rates struct {
	last  map[string]reading
	swept time.Time // the previous Sweep
}

type reading struct {
	value float64
	at    time.Time
}

// NewRates returns an empty tracker.
func NewRates() *Rates { return &Rates{last: map[string]reading{}} }

// Observe records value for key at the time it was read, and returns the
// per-second rate since the previous reading of key. ok is false when there
// is no usable previous reading: the first time a key is seen, after a reset,
// when no time has passed, or when either value is not finite.
func (r *Rates) Observe(key string, value float64, at time.Time) (rate float64, ok bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		// Not a reading. Forgetting the key, rather than storing it, means
		// the next finite value starts afresh instead of being subtracted
		// from NaN — which would lose that interval too.
		delete(r.last, key)
		return 0, false
	}
	prev, seen := r.last[key]
	r.last[key] = reading{value: value, at: at}
	if !seen {
		return 0, false
	}
	dt := at.Sub(prev.at).Seconds()
	if dt <= 0 || value < prev.value {
		return 0, false
	}
	return (value - prev.value) / dt, true
}

// Prune forgets every key last observed before cutoff. Collectors call it
// after each run with a cutoff of a few intervals ago, so a key that misses
// one run (a slow read) keeps its history but one that is gone for good does
// not accumulate.
func (r *Rates) Prune(cutoff time.Time) {
	for k, v := range r.last {
		if v.at.Before(cutoff) {
			delete(r.last, k)
		}
	}
}

// Forget drops every key for which drop returns true: for a collector that
// knows exactly which keys are gone (a container that left the list, one
// restarted in place whose counters began again) rather than inferring it
// from time, which [Rates.Prune] does on the readings' own clock.
func (r *Rates) Forget(drop func(key string) bool) {
	for k := range r.last {
		if drop(k) {
			delete(r.last, k)
		}
	}
}

// ForgetAfter is how long [Rates.Sweep] keeps a key that stopped
// appearing, at the least. Long enough to survive a few failed runs, short
// enough that a laptop's churn of tunnels and USB disks, or a host's churn
// of containers, does not accumulate.
const ForgetAfter = 5 * time.Minute

// Sweep is [Rates.Prune] for a collector that calls it once per run, at the
// run's start time: it forgets keys unseen for ForgetAfter, or for three
// times the gap since the previous Sweep if that is longer. With a long
// interval (10m) a fixed 5 minutes would forget, after one failed read,
// the readings the next run needs. The gap is measured rather than taken
// from configuration, where zero means "the scheduler's default".
func (r *Rates) Sweep(now time.Time) {
	keep := ForgetAfter
	if !r.swept.IsZero() {
		keep = max(keep, 3*now.Sub(r.swept))
	}
	r.swept = now
	r.Prune(now.Add(-keep))
}

// Len returns the number of keys being tracked.
func (r *Rates) Len() int { return len(r.last) }

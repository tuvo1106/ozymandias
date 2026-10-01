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

// Delta records value for key like [Rates.Observe], and returns the
// increase since the previous reading rather than a per-second rate: for a
// cumulative count sent as "events this interval" (a histogram's _count).
// A count that went down means the source restarted, and the new value is
// then what happened since, so it is the increase. ok is false for a first
// reading or a value that is not finite. A key is used with one of Observe
// or Delta, not both.
func (r *Rates) Delta(key string, value float64, at time.Time) (delta float64, ok bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		delete(r.last, key)
		return 0, false
	}
	prev, seen := r.last[key]
	r.last[key] = reading{value: value, at: at}
	switch {
	case !seen:
		return 0, false
	case value < prev.value:
		return value, true
	}
	return value - prev.value, true
}

// Change records value for key like [Rates.Delta], and returns the signed
// difference from the previous reading, with no reset rule: for a value
// that may legitimately fall (a histogram's _sum of negative observations),
// whose caller tells a restart from a fall by a companion that cannot (its
// _count). ok is false for a first reading or a value that is not finite.
func (r *Rates) Change(key string, value float64, at time.Time) (diff float64, ok bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		delete(r.last, key)
		return 0, false
	}
	prev, seen := r.last[key]
	r.last[key] = reading{value: value, at: at}
	if !seen {
		return 0, false
	}
	return value - prev.value, true
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
//
// It returns the cutoff it used, so a collector with per-series state of
// its own (the openmetrics check's histogram buckets) forgets that state
// by the same rule rather than a copy of it.
func (r *Rates) Sweep(now time.Time) (cutoff time.Time) {
	keep := ForgetAfter
	if !r.swept.IsZero() {
		keep = max(keep, 3*now.Sub(r.swept))
	}
	r.swept = now
	cutoff = now.Add(-keep)
	r.Prune(cutoff)
	return cutoff
}

// Len returns the number of keys being tracked.
func (r *Rates) Len() int { return len(r.last) }

// Has reports whether key has a reading: whether the next Observe of it
// updates state rather than adding it.
func (r *Rates) Has(key string) bool {
	_, ok := r.last[key]
	return ok
}

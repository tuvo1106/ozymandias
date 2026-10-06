package sampler

import (
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Defaults from the plan (docs/plan/M5-tracing.md §3).
const (
	DefaultTargetTPS  = 50.0
	DefaultErrorTPS   = 10.0
	DefaultRareTPS    = 5.0
	DefaultRareWindow = 5 * time.Minute
	DefaultRateWindow = 10 * time.Second
	maxRareEntries    = 20000
	maxRateKeys       = 500
)

// Reason names why a chunk was kept, for the self-metrics and the tests.
type Reason string

// The reasons a chunk is kept; Dropped (the empty reason) means no sampler kept it.
const (
	Dropped  Reason = ""
	Priority Reason = "priority"
	Error    Reason = "error"
	Rare     Reason = "rare"
)

// Options configure a Sampler.
type Options struct {
	// TargetTPS is the traces per second per service the head rate aims for.
	TargetTPS, ErrorTPS, RareTPS float64
	RareWindow, RateWindow       time.Duration
	// Env is the default env for the rate key of spans that carry none.
	Env      string
	Registry *selfmetrics.Registry
}

// Sampler is safe for concurrent use.
type Sampler struct {
	opts Options

	mu       sync.Mutex
	errTok   bucket
	rareTok  bucket
	rareSeen map[string]time.Time

	// rate feedback: counts in the open window, and the rates computed from the last closed one.
	winStart time.Time
	counts   map[string]int
	rates    map[string]float64

	kept    map[Reason]*selfmetrics.Counter
	dropped *selfmetrics.Counter
}

// New returns a Sampler.
func New(opts Options) *Sampler {
	if opts.TargetTPS <= 0 {
		opts.TargetTPS = DefaultTargetTPS
	}
	if opts.ErrorTPS <= 0 {
		opts.ErrorTPS = DefaultErrorTPS
	}
	if opts.RareTPS <= 0 {
		opts.RareTPS = DefaultRareTPS
	}
	if opts.RareWindow <= 0 {
		opts.RareWindow = DefaultRareWindow
	}
	if opts.RateWindow <= 0 {
		opts.RateWindow = DefaultRateWindow
	}
	if opts.Registry == nil {
		opts.Registry = selfmetrics.NewRegistry()
	}
	s := &Sampler{
		opts: opts, errTok: newBucket(opts.ErrorTPS), rareTok: newBucket(opts.RareTPS),
		rareSeen: map[string]time.Time{}, counts: map[string]int{}, rates: map[string]float64{},
		kept:    map[Reason]*selfmetrics.Counter{},
		dropped: opts.Registry.Counter("ozy.agent.traces.chunks_dropped"),
	}
	for _, r := range []Reason{Priority, Error, Rare} {
		s.kept[r] = opts.Registry.Counter("ozy.agent.traces.chunks_kept", "reason:"+string(r))
	}
	return s
}

// Decide says whether to keep chunk, and why. It also counts the chunk toward
// its service's observed rate, kept or not: the feedback must see the traffic
// the head sampler is trying to thin.
func (s *Sampler) Decide(chunk []wire.Span, now time.Time) Reason {
	if len(chunk) == 0 {
		return Dropped
	}
	prio, hasPrio, isErr := 0, false, false
	var rare []string
	for i := range chunk {
		sp := &chunk[i]
		if p, ok := sp.Metrics[wire.MetricSamplingPriority]; ok {
			hasPrio = true
			prio = max(prio, int(p))
			if p < 0 {
				prio = -1
				break
			}
		}
		if sp.Error == 1 {
			isErr = true
		}
		if sp.TopLevel() {
			rare = append(rare, sp.Service+"\x00"+sp.Name+"\x00"+sp.Resource)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count(&chunk[0], now)
	if hasPrio && prio < 0 {
		s.dropped.Inc()
		return Dropped
	}
	// Rare is evaluated even when another sampler keeps the chunk, so "first
	// seen" is recorded and does not fire again five minutes of traffic later.
	rareFirst := s.noteRare(rare, now)
	switch {
	case hasPrio && prio >= wire.PriorityAutoKeep:
		return s.keep(Priority)
	case isErr && s.errTok.take(now):
		return s.keep(Error)
	case rareFirst && s.rareTok.take(now):
		return s.keep(Rare)
	}
	s.dropped.Inc()
	return Dropped
}

func (s *Sampler) keep(r Reason) Reason { s.kept[r].Inc(); return r }

// noteRare records the keys and reports whether any was new within the window.
func (s *Sampler) noteRare(keys []string, now time.Time) bool {
	first := false
	for _, k := range keys {
		// The stamp moves only when it fires: steady traffic on a route must not
		// keep pushing its next "rare" sighting into the future.
		if t, ok := s.rareSeen[k]; !ok || now.Sub(t) >= s.opts.RareWindow {
			first = true
			s.rareSeen[k] = now
		}
	}
	if len(s.rareSeen) > maxRareEntries {
		for k, t := range s.rareSeen {
			if now.Sub(t) >= s.opts.RareWindow {
				delete(s.rareSeen, k)
			}
		}
		// Still over after sweeping the expired: the cap is a safety net against
		// unbounded resources, and a reset only costs some traces being "rare" twice.
		if len(s.rareSeen) > maxRareEntries {
			clear(s.rareSeen)
		}
	}
	return first
}

// RateKey is the key format of the response's rate_by_service (§B).
func RateKey(service, env string) string { return "service:" + service + ",env:" + env }

func (s *Sampler) count(first *wire.Span, now time.Time) {
	if s.winStart.IsZero() {
		s.winStart = now
	}
	if el := now.Sub(s.winStart); el >= s.opts.RateWindow {
		rates := make(map[string]float64, len(s.counts))
		for k, n := range s.counts {
			if observed := float64(n) / el.Seconds(); observed > s.opts.TargetTPS {
				rates[k] = s.opts.TargetTPS / observed
			} else {
				rates[k] = 1
			}
		}
		s.rates, s.counts, s.winStart = rates, map[string]int{}, now
	}
	env := first.Meta["env"]
	if env == "" {
		env = s.opts.Env
	}
	k := RateKey(first.Service, env)
	if _, ok := s.counts[k]; ok || len(s.counts) < maxRateKeys {
		s.counts[k]++
	}
}

// Rates is what the receiver returns as rate_by_service: the head rate for each
// service seen in the last closed window. A service not listed keeps its SDK
// default (1.0 unless OZY_TRACE_SAMPLE_RATE says otherwise).
func (s *Sampler) Rates() map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]float64, len(s.rates))
	for k, v := range s.rates {
		out[k] = v
	}
	return out
}

// bucket is a token bucket refilled from the caller's clock, holding at most
// one second of tokens: a burst never exceeds what the rate allows per second.
type bucket struct {
	rate, tokens float64
	last         time.Time
}

func newBucket(rate float64) bucket { return bucket{rate: rate, tokens: rate} }

func (b *bucket) take(now time.Time) bool {
	if !b.last.IsZero() && now.After(b.last) {
		b.tokens = min(b.rate, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	if now.After(b.last) {
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

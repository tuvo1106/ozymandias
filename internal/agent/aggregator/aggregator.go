package aggregator

import (
	"cmp"
	"context"
	"hash/maphash"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Kind is how the aggregator combines a context's samples within a bucket.
type Kind uint8

// Sample kinds, one per statsd type family (docs/wire-protocol.md §A).
const (
	Counter      Kind = iota + 1 // Σ value/rate → count
	Gauge                        // last value → gauge
	Set                          // distinct members → gauge
	Histogram                    // local stats → gauges + count
	Distribution                 // a DDSketch, sent whole on hop D
)

// Sample is one measurement handed to the aggregator by a source (the statsd
// server today; checks and trace stats later). Names and tags are raw: the
// aggregator is the one place they are normalized.
type Sample struct {
	Name      string
	Kind      Kind
	Value     float64
	SetMember string // Set only
	// SampleRate is the client's sampling rate in (0,1]; 0 means 1. A
	// counter's value is scaled by 1/rate, so "1 of every 10 events was
	// sent" still sums to the true count.
	SampleRate float64
	Tags       []string
	// Timestamp (unix seconds) is the client's claimed time, used only when
	// within ±TimestampTolerance of receipt; 0 means "now".
	Timestamp int64
}

// TimestampTolerance bounds how far a client-supplied |T may be from the
// agent's clock and still be trusted. Beyond it, the client's clock is
// presumed wrong and receive time wins.
const TimestampTolerance = 60 * time.Second

// Options configures an Aggregator. Zero values get the documented defaults.
type Options struct {
	Clock    clock.Clock           // default clock.Real()
	Registry *selfmetrics.Registry // default: a new registry
	// Hostname becomes the host tag on every context that doesn't carry one.
	Hostname string
	// Tags (agent-level, e.g. env:dev) are added to every context. They are
	// normalized like any other tag; invalid ones are dropped.
	Tags []string
	// FlushInterval is the bucket width. Default 10s; must be whole seconds.
	FlushInterval time.Duration
	// ContextExpiry is how long an idle context is remembered (and, for a
	// counter, zero-filled). Default 5m.
	ContextExpiry time.Duration
	// HistogramMaxSamples caps the values kept per histogram context per
	// bucket; beyond it the kept values are a uniform reservoir sample.
	// Default 10000.
	HistogramMaxSamples int
	// Alpha is the relative accuracy of the sketches a distribution builds.
	// Default sketch.DefaultAlpha (1%). It travels to ozyd with every
	// sketch, so changing it does not silently reinterpret what is already
	// stored — but two agents reporting one metric at different alphas
	// cannot be merged, so it is a fleet-wide setting in practice.
	Alpha float64
	// Shards spreads contexts over independently locked maps. Default 16.
	Shards int
	// Rand drives reservoir sampling. Default: a randomly seeded source.
	Rand *rand.Rand
}

// Aggregator turns samples into one point per context per bucket. It is safe
// for concurrent use: Add is called from every statsd worker at once, and
// Flush from the flush loop.
type Aggregator struct {
	clock     clock.Clock
	interval  int64 // bucket width, seconds
	expiry    int64 // seconds
	histCap   int
	alpha     float64
	hostTag   string
	agentTags []string
	seed      maphash.Seed
	shards    []*shard

	randMu sync.Mutex
	rand   *rand.Rand

	// watermark is the start of the oldest bucket that has not been flushed
	// yet. A sample for an older bucket arrived too late: its bucket is gone,
	// so it is counted in the current one instead (see Add).
	watermark atomic.Int64

	nContexts      atomic.Int64
	samplesDropped *selfmetrics.Counter
	tagsDropped    *selfmetrics.Counter
	lateSamples    *selfmetrics.Counter
	pointsFlushed  *selfmetrics.Counter
	flushDuration  *selfmetrics.Gauge
}

type shard struct {
	mu       sync.Mutex
	contexts map[string]*aggContext
}

// aggContext is one series identity (kind + name + tag set) and its open
// buckets.
type aggContext struct {
	name     string
	tags     []string
	kind     Kind
	buckets  map[int64]*bucket // by bucket start, unix seconds
	lastSeen int64             // receive time of the latest sample, unix seconds
	// next is the first bucket a counter has not emitted yet — the zero-fill
	// cursor — and lastData the start of the last bucket that had data.
	// Zeros are filled only within the expiry after lastData. Unused for
	// other kinds.
	next, lastData int64
}

// bucket accumulates one context's samples for one interval. Only the fields
// for the context's kind are used; one struct keeps the hot path free of
// interfaces and type switches on allocation.
type bucket struct {
	sum     float64 // counter: Σ value/rate. histogram: Σ value
	last    float64 // gauge
	members map[string]struct{}
	// histogram
	n        int64   // samples received (unscaled)
	weighted float64 // Σ 1/rate: the estimated true count
	min, max float64
	values   []float64
	// distribution: every observation, at a bounded cost and a bounded
	// error, instead of a sample of them.
	sketch *sketch.Sketch
}

// New returns an Aggregator.
func New(opts Options) *Aggregator {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Registry == nil {
		opts.Registry = selfmetrics.NewRegistry()
	}
	if opts.FlushInterval < time.Second {
		opts.FlushInterval = 10 * time.Second
	}
	if opts.ContextExpiry <= 0 {
		opts.ContextExpiry = 5 * time.Minute
	}
	if opts.HistogramMaxSamples <= 0 {
		opts.HistogramMaxSamples = 10000
	}
	if !(opts.Alpha > 0 && opts.Alpha < 1) {
		opts.Alpha = sketch.DefaultAlpha
	}
	if opts.Shards <= 0 {
		opts.Shards = 16
	}
	if opts.Rand == nil {
		opts.Rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())) //nolint:gosec // reservoir sampling, not security
	}
	a := &Aggregator{
		clock:    opts.Clock,
		interval: int64(opts.FlushInterval / time.Second),
		expiry:   int64(opts.ContextExpiry / time.Second),
		histCap:  opts.HistogramMaxSamples,
		alpha:    opts.Alpha,
		seed:     maphash.MakeSeed(),
		rand:     opts.Rand,

		samplesDropped: opts.Registry.Counter("ozy.agent.aggregator.samples_dropped"),
		tagsDropped:    opts.Registry.Counter("ozy.agent.aggregator.tags_dropped"),
		lateSamples:    opts.Registry.Counter("ozy.agent.aggregator.late_samples"),
		pointsFlushed:  opts.Registry.Counter("ozy.agent.aggregator.points_flushed"),
		flushDuration:  opts.Registry.Gauge("ozy.agent.aggregator.flush_duration_ms"),
	}
	opts.Registry.GaugeFunc("ozy.agent.aggregator.contexts", func() float64 { return float64(a.nContexts.Load()) })
	if opts.Hostname != "" {
		if t, ok := wire.NormalizeTag("host:" + opts.Hostname); ok {
			a.hostTag = t
		}
	}
	for _, t := range opts.Tags {
		if n, ok := wire.NormalizeTag(t); ok {
			a.agentTags = append(a.agentTags, n)
		}
	}
	a.shards = make([]*shard, opts.Shards)
	for i := range a.shards {
		a.shards[i] = &shard{contexts: map[string]*aggContext{}}
	}
	a.watermark.Store(math.MinInt64)
	return a
}

// Add records one sample received at now.
func (a *Aggregator) Add(s Sample, now time.Time) {
	name, ok := wire.NormalizeMetricName(s.Name)
	if !ok || s.Kind < Counter || s.Kind > Distribution {
		a.samplesDropped.Inc()
		return
	}
	tags := a.contextTags(s.Tags)
	key := contextKey(s.Kind, name, tags)

	nowS := now.Unix()
	ts := nowS
	if s.Timestamp != 0 && abs(s.Timestamp-nowS) <= int64(TimestampTolerance/time.Second) {
		ts = s.Timestamp
	}
	rate := s.SampleRate
	if !(rate > 0 && rate <= 1) {
		rate = 1
	}
	// Everything below assumes finite arithmetic, and a bucket cannot recover
	// from a NaN or an ±Inf: it survives every later add, and the flush
	// carrying it then fails to encode (wire.Point rejects non-finite), so
	// one poisoned context can discard a whole payload. The statsd parser
	// rejects non-finite values but not a sample rate whose reciprocal is
	// unusable — "x:1|c|@1e-320" scales one increment to +Inf. Drop such a
	// sample as unusable rather than clamping the rate, which would invent a
	// weight the client never asked for.
	if !finite(s.Value/rate) || !finite(1/rate) {
		a.samplesDropped.Inc()
		return
	}

	sh := a.shards[maphash.String(a.seed, key)%uint64(len(a.shards))]
	sh.mu.Lock()
	defer sh.mu.Unlock()

	// Read the watermark under the shard lock: Flush advances it before
	// taking any shard lock, so either this sample lands in a bucket that
	// flush is about to emit, or it sees the new watermark. It can never
	// land in a bucket that was already emitted.
	start := floorTo(ts, a.interval)
	if wm := a.watermark.Load(); start < wm {
		// The bucket this belongs to has been flushed. Counting it in the
		// oldest open bucket keeps every counter increment (Σ is preserved)
		// at the cost of shifting it in time by at most TimestampTolerance —
		// better than re-emitting a flushed timestamp, which the store
		// would treat as an overwrite and lose the earlier value.
		a.lateSamples.Inc()
		start = wm
	}

	c := sh.contexts[key]
	if c == nil {
		c = &aggContext{name: name, tags: tags, kind: s.Kind, buckets: map[int64]*bucket{}, next: start, lastData: start}
		sh.contexts[key] = c
		a.nContexts.Add(1)
	}
	if nowS > c.lastSeen {
		c.lastSeen = nowS
	}
	b := c.buckets[start]
	if b == nil {
		b = &bucket{min: math.Inf(1), max: math.Inf(-1)}
		c.buckets[start] = b
		if c.kind == Counter && start < c.next {
			c.next = start
		}
	}
	a.update(c.kind, b, s, rate)
}

func (a *Aggregator) update(kind Kind, b *bucket, s Sample, rate float64) {
	switch kind {
	case Counter:
		b.sum += s.Value / rate
	case Gauge:
		b.last = s.Value
	case Set:
		if b.members == nil {
			b.members = map[string]struct{}{}
		}
		b.members[s.SetMember] = struct{}{}
	case Distribution:
		// No reservoir and no cap on observations: a sketch is 2048 buckets
		// whether it has seen ten values or ten billion, and every one of
		// them is inside the relative error. That is the whole difference
		// between a distribution and a histogram here — a histogram's p95 is
		// this host's p95 of a sample, and cannot be combined with another
		// host's; a distribution's merges exactly.
		if b.sketch == nil {
			b.sketch = sketch.New(a.alpha)
		}
		// The weight is 1/rate, so a sampled client still describes the true
		// shape. AddWithCount refuses only what Add already refused, and Add
		// has been checked above.
		if err := b.sketch.AddWithCount(s.Value, 1/rate); err != nil {
			a.samplesDropped.Inc()
		}
	case Histogram:
		b.n++
		b.weighted += 1 / rate
		b.sum += s.Value
		b.min = min(b.min, s.Value)
		b.max = max(b.max, s.Value)
		if len(b.values) < a.histCap {
			b.values = append(b.values, s.Value)
			return
		}
		// Reservoir sampling (Algorithm R): keep each of the n values seen
		// with equal probability cap/n, in O(cap) memory. min, max, sum and
		// count stay exact; only the median and p95 become estimates.
		a.randMu.Lock()
		j := a.rand.Int64N(b.n)
		a.randMu.Unlock()
		if j < int64(a.histCap) {
			b.values[j] = s.Value
		}
	}
}

// contextTags normalizes a sample's tags, adds the agent's tags and host, and
// returns the canonical set. The host tag is added only when the sample has
// no host of its own: a client reporting on behalf of another machine (a
// cron script, a proxy) knows better than the agent does.
func (a *Aggregator) contextTags(raw []string) []string {
	tags := make([]string, 0, len(raw)+len(a.agentTags)+1)
	for _, t := range raw {
		if n, ok := wire.NormalizeTag(t); ok {
			tags = append(tags, n)
		} else {
			a.tagsDropped.Inc()
		}
	}
	tags = append(tags, a.agentTags...)
	if a.hostTag != "" && !wire.HasTagKey(tags, "host") {
		tags = append(tags, a.hostTag)
	}
	tags = wire.CanonicalTags(tags)
	if len(tags) > wire.MaxTagsPerPoint {
		// Deterministic (the set is sorted), so a context keeps one identity.
		a.tagsDropped.Add(int64(len(tags) - wire.MaxTagsPerPoint))
		tags = tags[:wire.MaxTagsPerPoint]
	}
	return slices.Clip(tags)
}

func contextKey(kind Kind, name string, tags []string) string {
	var b strings.Builder
	b.Grow(len(name) + 2 + 16*len(tags))
	b.WriteByte(byte('0' + kind))
	b.WriteString(name)
	b.WriteByte('|')
	for i, t := range tags {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(t)
	}
	return b.String()
}

// Flush emits every bucket that has closed by now (end <= now) and forgets
// contexts idle for longer than the expiry. With final set it also emits the
// open buckets — used once, at shutdown, when there is no later flush to wait
// for. (If the agent restarts within the same interval, the restarted agent's
// point for that bucket replaces this one in the store; losing a partial
// bucket on restart is the accepted cost.)
func (a *Aggregator) Flush(now time.Time, final bool) ([]wire.Series, []wire.SketchSeries) {
	began := time.Now()
	nowS := now.Unix()
	cutoff := floorTo(nowS, a.interval) // buckets starting before this are closed
	if cutoff > a.watermark.Load() {
		a.watermark.Store(cutoff)
	}

	var out []wire.Series
	var sketches []wire.SketchSeries
	for _, sh := range a.shards {
		sh.mu.Lock()
		for key, c := range sh.contexts {
			out, sketches = a.flushContext(out, sketches, c, cutoff, final)
			if len(c.buckets) == 0 && nowS-c.lastSeen > a.expiry && (c.kind != Counter || c.next > c.lastData+a.expiry) {
				delete(sh.contexts, key)
				a.nContexts.Add(-1)
			}
		}
		sh.mu.Unlock()
	}
	slices.SortFunc(out, func(x, y wire.Series) int {
		if c := strings.Compare(x.Metric, y.Metric); c != 0 {
			return c
		}
		return slices.Compare(x.Tags, y.Tags)
	})
	slices.SortFunc(sketches, func(x, y wire.SketchSeries) int {
		if c := strings.Compare(x.Metric, y.Metric); c != 0 {
			return c
		}
		return slices.Compare(x.Tags, y.Tags)
	})
	var n int64
	for _, s := range out {
		n += int64(len(s.Points))
	}
	for _, s := range sketches {
		n += int64(len(s.Points))
	}
	a.pointsFlushed.Add(n)
	a.flushDuration.Set(float64(time.Since(began).Microseconds()) / 1000)
	return out, sketches
}

func (a *Aggregator) flushContext(out []wire.Series, sketches []wire.SketchSeries, c *aggContext, cutoff int64, final bool) ([]wire.Series, []wire.SketchSeries) {
	starts := make([]int64, 0, len(c.buckets))
	for s := range c.buckets {
		if s < cutoff || final {
			starts = append(starts, s)
		}
	}
	slices.Sort(starts)

	if c.kind == Counter {
		return a.flushCounter(out, c, cutoff, starts), sketches
	}
	if len(starts) == 0 {
		return out, sketches
	}
	e := emitter{c: c, interval: a.interval}
	for _, s := range starts {
		b := c.buckets[s]
		delete(c.buckets, s)
		switch c.kind {
		case Gauge:
			e.add("", wire.KindGauge, s, b.last)
		case Set:
			e.add("", wire.KindGauge, s, float64(len(b.members)))
		case Distribution:
			// Nothing derived here: the four exact aggregates ride inside
			// the sketch, and ozyd writes them as .count/.sum/.min/.max so
			// there is one place that decides what they are called.
			if b.sketch != nil {
				e.addSketch(s, b.sketch)
			}
		case Histogram:
			slices.Sort(b.values)
			e.add(".avg", wire.KindGauge, s, b.sum/float64(b.n))
			e.add(".min", wire.KindGauge, s, b.min)
			e.add(".max", wire.KindGauge, s, b.max)
			e.add(".median", wire.KindGauge, s, percentile(b.values, 0.5))
			e.add(".95percentile", wire.KindGauge, s, percentile(b.values, 0.95))
			e.add(".count", wire.KindCount, s, b.weighted)
		}
	}
	return append(out, e.series()...), append(sketches, e.sketchSeries()...)
}

// flushCounter walks the zero-fill cursor up to cutoff: a closed bucket with
// data emits its sum, and one without emits 0 if it is within the expiry of
// the last bucket that had data. Zero, not a gap, because "no errors this interval" and "no
// data" are different facts, and a monitor on error count needs to tell them
// apart. Gauges are never zero-filled: a gauge's absence means "unknown", and
// inventing a value would be a lie.
func (a *Aggregator) flushCounter(out []wire.Series, c *aggContext, cutoff int64, starts []int64) []wire.Series {
	e := emitter{c: c, interval: a.interval}
	// No bucket starts before next: Add lowers next when one would.
	s, i := c.next, 0
	for s < cutoff {
		if b, ok := c.buckets[s]; ok {
			e.add("", wire.KindCount, s, b.sum)
			delete(c.buckets, s)
			c.lastData = s
		} else if s <= c.lastData+a.expiry {
			e.add("", wire.KindCount, s, 0)
		} else {
			// Past the zero-fill horizon: jump straight to the next bucket
			// with data, so a long idle gap costs O(1), not O(gap/interval).
			for i < len(starts) && starts[i] <= s {
				i++
			}
			if i == len(starts) || starts[i] >= cutoff {
				s = cutoff
				break
			}
			s = starts[i]
			continue
		}
		s += a.interval
	}
	if s > c.next {
		c.next = s
	}
	// final: whatever is still open (the current bucket, or a future |T).
	for _, st := range starts {
		if b, ok := c.buckets[st]; ok {
			e.add("", wire.KindCount, st, b.sum)
			delete(c.buckets, st)
		}
	}
	return append(out, e.series()...)
}

// emitter collects one context's points per derived metric name.
type emitter struct {
	c        *aggContext
	interval int64
	order    []string
	bySuffix map[string]*wire.Series
	sketch   *wire.SketchSeries
}

// addSketch files one bucket's sketch. A distribution emits no derived
// series of its own: count, sum, min and max ride inside the sketch, and
// ozyd writes them as .count/.sum/.min/.max so that one place decides what
// they are called.
func (e *emitter) addSketch(ts int64, s *sketch.Sketch) {
	if e.sketch == nil {
		e.sketch = &wire.SketchSeries{Metric: e.c.name, Tags: e.c.tags, Interval: e.interval}
	}
	e.sketch.Points = append(e.sketch.Points, wire.SketchPoint{Timestamp: ts, Sketch: s.ToWire()})
}

func (e *emitter) sketchSeries() []wire.SketchSeries {
	if e.sketch == nil {
		return nil
	}
	slices.SortFunc(e.sketch.Points, func(x, y wire.SketchPoint) int {
		return cmp.Compare(x.Timestamp, y.Timestamp)
	})
	return []wire.SketchSeries{*e.sketch}
}

func (e *emitter) add(suffix string, kind wire.Kind, ts int64, v float64) {
	if e.bySuffix == nil {
		e.bySuffix = map[string]*wire.Series{}
	}
	s := e.bySuffix[suffix]
	if s == nil {
		s = &wire.Series{Metric: e.c.name + suffix, Type: kind, Tags: e.c.tags}
		if kind != wire.KindGauge {
			s.Interval = e.interval
		}
		e.bySuffix[suffix] = s
		e.order = append(e.order, suffix)
	}
	s.Points = append(s.Points, wire.Point{Timestamp: ts, Value: v})
}

func (e *emitter) series() []wire.Series {
	out := make([]wire.Series, 0, len(e.order))
	for _, k := range e.order {
		s := e.bySuffix[k]
		slices.SortFunc(s.Points, func(x, y wire.Point) int { return int(x.Timestamp - y.Timestamp) })
		out = append(out, *s)
	}
	return out
}

// Run flushes on every bucket boundary until ctx ends, handing each flush to
// sink — even an empty one, so the sink can add series of its own (the
// agent's self-metrics) on the same cadence — then does a final flush of
// the open buckets. It checks the clock once a second rather than ticking
// once per interval: a ticker started at an arbitrary phase would delay
// every bucket by up to a whole interval.
func (a *Aggregator) Run(ctx context.Context, sink func([]wire.Series, []wire.SketchSeries)) {
	t := a.clock.NewTicker(time.Second)
	defer t.Stop()
	last := floorTo(a.clock.Now().Unix(), a.interval)
	for {
		select {
		case <-ctx.Done():
			sink(a.Flush(a.clock.Now(), true))
			return
		case <-t.C():
			// Read the clock rather than trusting the tick's value: a ticker
			// whose receiver fell behind delivers a stale tick and drops the
			// rest, and the stale time could hold back a due flush.
			now := a.clock.Now()
			if c := floorTo(now.Unix(), a.interval); c > last {
				last = c
				sink(a.Flush(now, false))
			}
		}
	}
}

// Contexts returns the number of live contexts.
func (a *Aggregator) Contexts() int { return int(a.nContexts.Load()) }

// percentile is nearest-rank on sorted values: the smallest value with at
// least p of the data at or below it. Chosen over interpolation because it
// always returns a value that was actually observed.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return math.NaN()
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(i, 0)]
}

// finite reports whether v is a real number. See Add: the aggregator has no
// way to recover from a NaN or an ±Inf once one is in a bucket.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func floorTo(t, width int64) int64 {
	q := t / width
	if t%width != 0 && t < 0 {
		q--
	}
	return q * width
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

package collector

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tuvo1106/ozymandias/internal/agent/agenttags"
	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Defaults for [Options].
const (
	DefaultInterval        = 15 * time.Second
	DefaultTimeout         = 10 * time.Second
	DefaultShutdownTimeout = 2 * time.Second
)

// Options configures a [Scheduler]. Zero values get the defaults.
type Options struct {
	Collectors []Collector
	// Interval is the run period for a collector whose Interval() is zero.
	Interval time.Duration
	// Timeout bounds one Collect call. It is capped at the collector's
	// interval: a run that could outlast its period would overlap the next
	// one's slot, and the ticker would silently drop that run instead.
	Timeout time.Duration
	// ShutdownTimeout is how long Run waits, once its context is cancelled,
	// for collectors to return. Short, because it is spent inside the
	// agent's stop grace period: a collector honouring its context returns
	// at once, and one that does not will not return in time anyway.
	ShutdownTimeout time.Duration
	// Sink receives each run's series. It must not block for long — the
	// forwarder's Submit only queues — and is called from one goroutine per
	// collector, so it must be safe for concurrent use.
	Sink func([]wire.Series)
	// HostTag ("host:<name>", already normalized) and Tags (the agent's
	// global tags) are added to every series, as the aggregator adds them to
	// statsd series: a collector says what it measured, not where.
	HostTag string
	Tags    []string

	Clock    clock.Clock           // default clock.Real()
	Registry *selfmetrics.Registry // default: a new registry
	Logger   *slog.Logger          // default slog.Default()
	Rand     *rand.Rand            // start-time jitter; default randomly seeded
}

// Scheduler runs collectors, each on its own goroutine and interval, and
// hands their output to a sink.
//
// One goroutine per collector, rather than one loop running them in turn, is
// what makes "a failing or slow collector never delays the others" true by
// construction: a Docker daemon that takes nine seconds to answer holds up
// the Docker collector and nothing else.
//
// Each collector starts after a random delay within its first interval, so
// that ten checks on the same 15s interval spread across it instead of all
// firing on the same tick — the same reason cron jobs get jitter. After that
// the period is fixed, so the spacing between two readings (the denominator
// of every rate) stays the interval.
type Scheduler struct {
	opts    Options
	log     *slog.Logger
	hostTag string
	tags    []string // Tags, normalized once
	randMu  sync.Mutex
	// stopped is set when Run returns. A run still in progress then — a
	// collector abandoned at shutdown — sends nothing: its sink is being
	// shut down, and would queue what it was given where nothing sends it.
	stopped atomic.Bool
}

// New returns a Scheduler. Call Run to start it.
func New(opts Options) *Scheduler {
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = DefaultShutdownTimeout
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Registry == nil {
		opts.Registry = selfmetrics.NewRegistry()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Rand == nil {
		opts.Rand = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	if opts.Sink == nil {
		opts.Sink = func([]wire.Series) {}
	}
	s := &Scheduler{opts: opts, log: opts.Logger.With("component", "collector"), hostTag: opts.HostTag}
	for _, t := range opts.Tags {
		if n, ok := wire.NormalizeTag(t); ok {
			s.tags = append(s.tags, n)
		}
	}
	return s
}

// Run starts every collector and blocks until ctx is cancelled and every
// collector's goroutine has returned — or, after cancellation, until
// Options.ShutdownTimeout has passed, whichever is first. A Collect in progress at
// cancellation sees its context cancelled and its partial batch is still
// sent, so shutdown loses at most the run that was interrupted.
//
// The bound is there because cancellation cannot interrupt everything: a
// statfs on a hung disk ignores its context. Without it, one stuck mount
// would hold the agent's shutdown until the container was killed, and the
// aggregator's final flush — every statsd bucket still open — would never
// run. A collector still running past the bound is abandoned (logged); its
// goroutine ends with the process, and whatever it collects is dropped.
func (s *Scheduler) Run(ctx context.Context) {
	defer s.stopped.Store(true)
	var wg sync.WaitGroup
	// A slice, not a map by name: several instances of one check share a
	// name.
	alive := make([]atomic.Bool, len(s.opts.Collectors))
	for i, c := range s.opts.Collectors {
		alive := &alive[i]
		alive.Store(true)
		wg.Go(func() {
			defer alive.Store(false)
			s.loop(ctx, c)
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-ctx.Done():
	}
	t := s.opts.Clock.NewTimer(s.opts.ShutdownTimeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C():
		for i := range alive {
			if alive[i].Load() {
				s.log.Warn("collector did not stop in time; abandoning it", "collector", s.opts.Collectors[i].Name(), "waited", s.opts.ShutdownTimeout)
			}
		}
	}
}

// interval is c's period, rounded up to whole seconds (a count's interval is
// whole seconds on the wire) and at least one.
func (s *Scheduler) interval(c Collector) time.Duration {
	iv := c.Interval()
	if iv <= 0 {
		iv = s.opts.Interval
	}
	if r := iv % time.Second; r != 0 {
		iv += time.Second - r
	}
	return max(iv, time.Second)
}

func (s *Scheduler) jitter(iv time.Duration) time.Duration {
	s.randMu.Lock()
	defer s.randMu.Unlock()
	return time.Duration(s.opts.Rand.Int64N(int64(iv)))
}

// loop runs one collector until ctx is done.
func (s *Scheduler) loop(ctx context.Context, c Collector) {
	iv := s.interval(c)
	st := &runState{
		name: c.Name(),
		runs: s.opts.Registry.Counter("ozy.agent.collector.runs", "collector:"+c.Name()),
		errs: s.opts.Registry.Counter("ozy.agent.collector.errors", "collector:"+c.Name()),
		tout: s.opts.Registry.Counter("ozy.agent.collector.timeouts", "collector:"+c.Name()),
		pts:  s.opts.Registry.Counter("ozy.agent.collector.points", "collector:"+c.Name()),
		drop: s.opts.Registry.Counter("ozy.agent.collector.dropped", "collector:"+c.Name()),
		tags: s.opts.Registry.Counter("ozy.agent.collector.tags_dropped", "collector:"+c.Name()),
		dur:  s.opts.Registry.Gauge("ozy.agent.collector.duration_ms", "collector:"+c.Name()),
	}

	first := s.opts.Clock.NewTimer(s.jitter(iv))
	select {
	case <-ctx.Done():
		first.Stop()
		return
	case <-first.C():
	}
	tick := s.opts.Clock.NewTicker(iv)
	defer tick.Stop()
	for {
		s.runOnce(ctx, c, iv, st)
		select {
		case <-ctx.Done():
			return
		case <-tick.C():
		}
	}
}

// runState is one collector's instruments and the error it last logged.
type runState struct {
	name                        string
	runs, errs, tout, pts, drop *selfmetrics.Counter
	tags                        *selfmetrics.Counter
	dur                         *selfmetrics.Gauge
	lastErr                     string
}

// runOnce calls Collect once and sends what it emitted.
func (s *Scheduler) runOnce(ctx context.Context, c Collector, iv time.Duration, st *runState) {
	start := s.opts.Clock.Now()
	ts := start.Unix()
	cctx, cancel := context.WithTimeout(ctx, min(s.opts.Timeout, iv))
	defer cancel()

	var (
		mu    sync.Mutex
		open  = true
		batch []wire.Series
	)
	emit := func(m Metric) {
		mu.Lock()
		defer mu.Unlock()
		if !open {
			// A goroutine the collector left behind. Its batch is gone.
			st.drop.Inc()
			return
		}
		se, badTags, ok := s.series(m, ts, iv)
		st.tags.Add(int64(badTags))
		if ok {
			batch = append(batch, se)
		} else {
			st.drop.Inc()
		}
	}
	err := c.Collect(cctx, emit)
	mu.Lock()
	open = false
	out := batch
	mu.Unlock()

	st.runs.Inc()
	st.dur.Set(float64(s.opts.Clock.Now().Sub(start)) / float64(time.Millisecond))
	st.pts.Add(int64(len(out)))
	if errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
		st.tout.Inc()
	}
	s.report(ctx, st, err)
	if s.stopped.Load() {
		st.drop.Add(int64(len(out)))
		return
	}
	if len(out) > 0 {
		s.opts.Sink(out)
	}
}

// report logs a failing run on the transition into failure, or when the
// error changes, and once on recovery — not every interval. A check pointed
// at a service that is down would otherwise write a warning every 15 seconds
// for as long as it is down; the errors counter still records every run.
func (s *Scheduler) report(ctx context.Context, st *runState, err error) {
	if err != nil {
		st.errs.Inc()
		if ctx.Err() != nil {
			return // shutting down; not the collector's fault
		}
		if msg := err.Error(); msg != st.lastErr {
			s.log.Warn("collector failed", "collector", st.name, "error", err)
			st.lastErr = msg
		}
		return
	}
	if st.lastErr != "" {
		s.log.Info("collector recovered", "collector", st.name)
		st.lastErr = ""
	}
}

// series turns one emitted metric into a wire series, or reports false if it
// cannot be sent: a name the intake would refuse, or a value with no JSON
// form. A bad tag drops the tag, not the metric — the same trade the
// aggregator makes for statsd.
func (s *Scheduler) series(m Metric, ts int64, iv time.Duration) (se wire.Series, badTags int, ok bool) {
	name, ok := wire.NormalizeMetricName(m.Name)
	if !ok || math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
		return wire.Series{}, 0, false
	}
	tags := make([]string, 0, len(m.Tags)+len(s.tags))
	for _, t := range m.Tags {
		if n, ok := wire.NormalizeTag(t); ok {
			tags = append(tags, n)
		} else {
			badTags++
		}
	}
	// The same decoration as statsd series get (agenttags): a check that
	// measures another machine says host:db1 and keeps it.
	tags, capped := agenttags.Decorate(tags, s.tags, s.hostTag)
	badTags += capped
	se = wire.Series{Metric: name, Tags: tags, Points: []wire.Point{{Timestamp: ts, Value: m.Value}}}
	switch m.Kind {
	case Count:
		se.Type, se.Interval = wire.KindCount, int64(iv/time.Second)
	case Gauge, Rate: // a per-second value is a level; see Rate
		se.Type = wire.KindGauge
	default:
		return wire.Series{}, badTags, false
	}
	return se, badTags, true
}

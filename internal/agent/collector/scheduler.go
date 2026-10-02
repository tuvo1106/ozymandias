package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"math/rand/v2"
	"runtime/debug"
	"slices"
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
	// SketchSink receives each run's distributions, under the same rules as
	// Sink. Nil drops them, counted.
	SketchSink func([]wire.SketchSeries)
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
	// sinkMu makes the check and the send one step, so a run that finishes
	// just as Run gives up either sends before Run returns or not at all.
	// Runs hold it shared, so collectors still send (encode, gzip) in
	// parallel; only Run's stop takes it exclusively.
	sinkMu  sync.RWMutex
	stopped bool

	// The running set. Before Run, pending holds what Add was given; while
	// Run runs, each collector is a task on wg. closing is set when Run
	// stops accepting, after which Add does nothing.
	mu      sync.Mutex
	ctx     context.Context
	pending []*task
	running map[*task]bool
	closing bool
	wg      sync.WaitGroup
}

// task is one collector's goroutine.
type task struct {
	c      Collector
	cancel context.CancelFunc
	alive  atomic.Bool
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
	s := &Scheduler{opts: opts, log: opts.Logger.With("component", "collector"), hostTag: opts.HostTag, running: map[*task]bool{}}
	for _, c := range opts.Collectors {
		s.Add(c)
	}
	s.tags = agenttags.Normalize(opts.Tags)
	return s
}

// Collectors returns the collectors the scheduler runs, or will once Run
// starts.
func (s *Scheduler) Collectors() []Collector {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Collector
	for _, t := range s.pending {
		out = append(out, t.c)
	}
	for t := range s.running {
		out = append(out, t.c)
	}
	return out
}

// Add schedules c — before Run, or while it runs (autodiscovery adds a check
// when a container asking for one starts) — and returns a function that
// stops it again. Stopping cancels its context; a run in progress is cut
// short and not sent (counted in ozy.agent.collector.dropped), since what
// it reads of its own cancellation is not a reading of the source. After
// Run has begun shutting down, Add does nothing.
//
// Collectors that share a name share its self-metrics: counts add up, and
// the duration is the latest run's. That is what autodiscovery wants when
// several containers fold into one name (container_name_rewrite), and what
// makes a recreated container's check start at once rather than wait for
// its predecessor's goroutine to wind down. Configured instances cannot
// share a name; the registry refuses that at startup. A collector's
// self-metrics are released when its loop ends, so they last as long as
// some collector of that name runs, plus one report.
func (s *Scheduler) Add(c Collector) (remove func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return func() {}
	}
	t := &task{c: c}
	if s.ctx == nil {
		s.pending = append(s.pending, t)
	} else {
		s.startLocked(t)
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if i := slices.Index(s.pending, t); i >= 0 {
			s.pending = slices.Delete(s.pending, i, i+1)
			return
		}
		if t.cancel != nil {
			t.cancel()
		}
	}
}

// startLocked starts t's goroutine. s.mu is held.
func (s *Scheduler) startLocked(t *task) {
	ctx, cancel := context.WithCancel(s.ctx)
	t.cancel = cancel
	t.alive.Store(true)
	s.running[t] = true
	s.wg.Go(func() {
		defer func() {
			cancel()
			t.alive.Store(false)
			s.mu.Lock()
			delete(s.running, t)
			s.mu.Unlock()
		}()
		s.loop(ctx, t.c)
	})
}

// Run is called once. It starts every collector and blocks until ctx is
// cancelled and every collector's goroutine has returned — or, after
// cancellation, until Options.ShutdownTimeout has passed, whichever is
// first. A Collect in progress at
// cancellation sees its context cancelled and its batch is not sent: what
// it read after the cancellation describes the cancellation, not the
// source. Shutdown loses at most the run that was interrupted.
//
// The bound is there because cancellation cannot interrupt everything: a
// statfs on a hung disk ignores its context. Without it, one stuck mount
// would hold the agent's shutdown until the container was killed, and the
// aggregator's final flush — every statsd bucket still open — would never
// run. A collector still running past the bound is abandoned (logged); its
// goroutine ends with the process, and whatever it collects is dropped.
//
// Abandoned means leaked: Go cannot stop a goroutine from outside, and one
// that ignores its context — which is what being abandoned means — runs
// until it returns or the process exits. Its loop then sees ctx cancelled
// and ends, as does the goroutine waiting for it. In the agent the process
// exits moments later; an embedder that outlives Run should expect up to
// two goroutines per stuck collector.
func (s *Scheduler) Run(ctx context.Context) {
	s.mu.Lock()
	if s.ctx != nil || s.closing {
		s.mu.Unlock()
		return // Run is called once; a second call must not stop the first's sink
	}
	defer func() {
		s.sinkMu.Lock()
		s.stopped = true
		s.sinkMu.Unlock()
	}()
	s.ctx = ctx
	for _, t := range s.pending {
		s.startLocked(t)
	}
	s.pending = nil
	s.mu.Unlock()

	<-ctx.Done()
	s.mu.Lock()
	s.closing = true // no Add from here on, so wg only shrinks
	stuck := slices.Collect(maps.Keys(s.running))
	s.mu.Unlock()

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	t := s.opts.Clock.NewTimer(s.opts.ShutdownTimeout)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C():
		for _, st := range stuck {
			if st.alive.Load() {
				s.log.Warn("collector did not stop in time; abandoning it", "collector", st.c.Name(), "waited", s.opts.ShutdownTimeout)
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

// The self-metrics every collector has, each tagged collector:<name>.
// Named, not indexed: the counter a field holds and the one released for
// it must be the same, whatever order a later change lists them in.
const (
	selfRuns        = "ozy.agent.collector.runs"
	selfErrors      = "ozy.agent.collector.errors"
	selfTimeouts    = "ozy.agent.collector.timeouts"
	selfPoints      = "ozy.agent.collector.points"
	selfDropped     = "ozy.agent.collector.dropped"
	selfTagsDropped = "ozy.agent.collector.tags_dropped"
	selfDuration    = "ozy.agent.collector.duration_ms"
)

// loop runs one collector until ctx is done.
func (s *Scheduler) loop(ctx context.Context, c Collector) {
	iv := s.interval(c)
	reg, tag := s.opts.Registry, "collector:"+c.Name()
	st := &runState{
		name: c.Name(),
		runs: reg.Counter(selfRuns, tag),
		errs: reg.Counter(selfErrors, tag),
		tout: reg.Counter(selfTimeouts, tag),
		pts:  reg.Counter(selfPoints, tag),
		drop: reg.Counter(selfDropped, tag),
		tags: reg.Counter(selfTagsDropped, tag),
		dur:  reg.Gauge(selfDuration, tag),
	}
	defer func() {
		for _, n := range []string{selfRuns, selfErrors, selfTimeouts, selfPoints, selfDropped, selfTagsDropped, selfDuration} {
			reg.Release(n, tag)
		}
	}()

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
	// The timeout runs on the injected clock, like everything else here, so
	// the duration gauge and the timeout agree on how long a run took. It is
	// a timeout only if it fires before Collect returns: a run that
	// finished at 9.999s of 10 succeeded.
	limit := min(s.opts.Timeout, iv)
	cctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var (
		tmu               sync.Mutex
		finished, expired bool
	)
	timer := s.opts.Clock.NewTimer(limit)
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		select {
		case <-timer.C():
			tmu.Lock()
			if !finished {
				expired = true
				cancel(context.DeadlineExceeded)
			}
			tmu.Unlock()
		case <-cctx.Done():
		}
	}()

	var (
		mu       sync.Mutex
		open     = true
		batch    []wire.Series
		sketches []wire.SketchSeries
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
		switch {
		case !ok:
			st.drop.Inc()
		case m.Kind == Distribution:
			sketches = append(sketches, wire.SketchSeries{
				Metric: se.Metric, Tags: se.Tags, Interval: int64(iv / time.Second),
				Points: []wire.SketchPoint{{Timestamp: ts, Sketch: m.Sketch.ToWire()}},
			})
		default:
			batch = append(batch, se)
		}
	}
	err := s.collect(cctx, c, emit)
	tmu.Lock()
	finished = true
	timedOut := expired
	tmu.Unlock()
	timer.Stop()
	cancel(nil)
	<-watching
	if timedOut && err != nil {
		// The collector saw its context cancelled (the timer is the
		// injected clock's, so the context has no deadline of its own);
		// the error says what happened, and matches DeadlineExceeded.
		if errors.Is(err, context.Canceled) {
			err = fmt.Errorf("timed out after %v: %w", limit, context.DeadlineExceeded)
		} else {
			err = fmt.Errorf("timed out after %v: %w", limit, err)
		}
	}
	mu.Lock()
	open = false
	out, outSketches := batch, sketches
	mu.Unlock()

	st.runs.Inc()
	st.dur.Set(float64(s.opts.Clock.Now().Sub(start)) / float64(time.Millisecond))
	st.pts.Add(int64(len(out) + len(outSketches)))
	if timedOut && ctx.Err() == nil {
		st.tout.Inc()
	}
	s.report(ctx, st, err)
	if ctx.Err() != nil {
		// Stopped mid-run: the collector was removed (its container went,
		// or its settings changed) or the agent is shutting down. What the
		// run read after that is a reading of its own cancellation — a
		// check whose dial was cancelled reports can_connect 0 — and would
		// put a false outage on every restart. The run is not sent; a
		// timeout, by contrast, is a real reading of a slow source, and is.
		st.drop.Add(int64(len(out) + len(outSketches)))
		return
	}
	s.sinkMu.RLock()
	defer s.sinkMu.RUnlock()
	if s.stopped {
		st.drop.Add(int64(len(out) + len(outSketches)))
		return
	}
	if len(out) > 0 {
		s.opts.Sink(out) // must not block: Run's return waits for sinkMu
	}
	if len(outSketches) > 0 {
		if s.opts.SketchSink == nil {
			st.drop.Add(int64(len(outSketches)))
		} else {
			s.opts.SketchSink(outSketches)
		}
	}
}

// collect runs c.Collect, turning a panic into the run's error. A check
// parses what a server it does not control sends, and the agent also
// carries statsd, traces and every other collector: one bug in one check's
// parser must cost that check's run, not the process. The stack is logged,
// since the error alone would not say where to look. A panic in a goroutine
// the collector starts itself is beyond this; a collector that fans out
// recovers in its own goroutines.
func (s *Scheduler) collect(ctx context.Context, c Collector, emit Emit) (err error) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("collector panicked; the run counts as failed", "collector", c.Name(), "panic", p, "stack", string(debug.Stack()))
			err = fmt.Errorf("panicked: %v", p)
		}
	}()
	return c.Collect(ctx, emit)
}

// report logs a failing run on the transition into failure, or when the
// error changes, and once on recovery — not every interval. A check pointed
// at a service that is down would otherwise write a warning every 15 seconds
// for as long as it is down; the errors counter still records every run.
func (s *Scheduler) report(ctx context.Context, st *runState, err error) {
	if err != nil {
		if ctx.Err() != nil {
			// Shutting down: the run was cut short, not failed. Counting it
			// would put an error on the graph at every restart.
			return
		}
		st.errs.Inc()
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
	norm := func(in []string) []string {
		out := make([]string, 0, len(in))
		for _, t := range in {
			if n, ok := wire.NormalizeTag(t); ok {
				out = append(out, n)
			} else {
				badTags++
			}
		}
		return out
	}
	// The same decoration as statsd series get (agenttags): a check that
	// measures another machine says host:db1 and keeps it. Keep survives
	// the tag cap; Tags give way first.
	tags, capped := agenttags.DecorateKeeping(norm(m.Tags), norm(m.Keep), s.tags, s.hostTag)
	badTags += capped
	se = wire.Series{Metric: name, Tags: tags, Points: []wire.Point{{Timestamp: ts, Value: m.Value}}}
	switch m.Kind {
	case Count:
		se.Type, se.Interval = wire.KindCount, int64(iv/time.Second)
	case Gauge, Rate: // a per-second value is a level; see Rate
		se.Type = wire.KindGauge
	case Distribution:
		if m.Sketch == nil || m.Sketch.Count() == 0 {
			return wire.Series{}, badTags, false
		}
	default:
		return wire.Series{}, badTags, false
	}
	return se, badTags, true
}

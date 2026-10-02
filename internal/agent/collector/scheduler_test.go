package collector

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// fake is a Collector whose Collect is whatever the test says.
type fake struct {
	name    string
	iv      time.Duration
	collect func(ctx context.Context, emit Emit) error
	calls   atomic.Int32
}

func (f *fake) Name() string            { return f.name }
func (f *fake) Interval() time.Duration { return f.iv }
func (f *fake) Collect(ctx context.Context, emit Emit) error {
	f.calls.Add(1)
	return f.collect(ctx, emit)
}

// sink collects what the scheduler sends.
type sink struct {
	mu      sync.Mutex
	batches [][]wire.Series
}

func (s *sink) send(b []wire.Series) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, b)
}

func (s *sink) all() []wire.Series {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []wire.Series
	for _, b := range s.batches {
		out = append(out, b...)
	}
	return out
}

// counter reads a collector's self-metric counter without keeping a hold
// on it, so reading does not keep it alive past its collectors.
func counter(reg *selfmetrics.Registry, name, coll string) int64 {
	defer reg.Release(name, "collector:"+coll)
	return reg.Counter(name, "collector:"+coll).Value()
}

// start runs a scheduler on a fake clock until the test ends.
func start(t *testing.T, opts Options) (*testutil.FakeClock, *sink, *selfmetrics.Registry) {
	t.Helper()
	fc := testutil.NewFakeClock(t0)
	sk := &sink{}
	reg := selfmetrics.NewRegistry()
	opts.Clock, opts.Registry, opts.Sink = fc, reg, sk.send
	if opts.Rand == nil {
		opts.Rand = rand.New(rand.NewPCG(1, 2))
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	s := New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return fc, sk, reg
}

func TestScheduler_SendsWhatACollectorEmits(t *testing.T) {
	c := &fake{name: "host", iv: 15 * time.Second, collect: func(_ context.Context, emit Emit) error {
		emit(Metric{Name: "system.load.1", Kind: Gauge, Value: 0.5, Tags: []string{"Device:SDA"}})
		emit(Metric{Name: "system.net.bytes_rcvd", Kind: Rate, Value: 1024, Tags: []string{"interface:en0"}})
		emit(Metric{Name: "container.exits", Kind: Count, Value: 1, Tags: []string{"host:mac"}}) // not twice
		return nil
	}}
	fc, sk, _ := start(t, Options{Collectors: []Collector{c}, HostTag: "host:mac", Tags: []string{"Env:Dev", "k:a,b"}})
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "jitter timer not armed")
	fc.Advance(15 * time.Second) // past any jitter within the first interval
	testutil.Eventually(t, time.Second, func() bool { return len(sk.all()) == 3 }, "batch not sent")

	got := sk.all()
	want := []wire.Series{
		{Metric: "system.load.1", Type: wire.KindGauge, Tags: []string{"device:sda", "env:dev", "host:mac"}},
		{Metric: "system.net.bytes_rcvd", Type: wire.KindGauge, Tags: []string{"env:dev", "host:mac", "interface:en0"}}, // ADR-0026
		{Metric: "container.exits", Type: wire.KindCount, Interval: 15, Tags: []string{"env:dev", "host:mac"}},
	}
	for i, w := range want {
		g := got[i]
		if g.Metric != w.Metric || g.Type != w.Type || g.Interval != w.Interval || !slices.Equal(g.Tags, w.Tags) {
			t.Errorf("series %d = %+v, want %+v", i, g, w)
		}
		if len(g.Points) != 1 || g.Points[0].Timestamp < t0.Unix() || g.Points[0].Timestamp > t0.Add(15*time.Second).Unix() {
			t.Errorf("series %d points = %v", i, g.Points)
		}
	}
}

func TestScheduler_RunsOnItsIntervalAfterJitter(t *testing.T) {
	c := &fake{name: "c", iv: 10 * time.Second, collect: func(context.Context, Emit) error { return nil }}
	fc, _, reg := start(t, Options{Collectors: []Collector{c}})
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "not armed")
	fc.Advance(10 * time.Second) // the jitter is within [0, 10s)
	testutil.Eventually(t, time.Second, func() bool { return c.calls.Load() == 1 }, "first run")
	for want := int32(2); want <= 4; want++ {
		testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "ticker not armed")
		fc.Advance(10 * time.Second)
		testutil.Eventually(t, time.Second, func() bool { return c.calls.Load() == want }, "run %d", want)
	}
	if n := counter(reg, "ozy.agent.collector.runs", "c"); n != 4 {
		t.Fatalf("runs counter = %d, want 4", n)
	}
}

// The reason for one goroutine per collector: a collector stuck in its
// source does not stop another from running on time.
func TestScheduler_ASlowCollectorDoesNotDelayAnother(t *testing.T) {
	release := make(chan struct{})
	// Stuck for good: it ignores its context, so even its timeout (its
	// interval, 1s) does not free it.
	slow := &fake{name: "slow", iv: time.Second, collect: func(context.Context, Emit) error {
		<-release
		return nil
	}}
	quick := &fake{name: "quick", iv: time.Second, collect: func(context.Context, Emit) error { return nil }}
	fc, _, _ := start(t, Options{Collectors: []Collector{slow, quick}, Timeout: time.Hour})
	defer close(release)
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 2 }, "not armed")
	fc.Advance(time.Second)
	testutil.Eventually(t, time.Second, func() bool { return slow.calls.Load() == 1 && quick.calls.Load() == 1 }, "first runs")
	for want := int32(2); want <= 5; want++ {
		// slow holds its goroutine, so only quick's ticker is waiting.
		testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() >= 2 }, "quick's ticker")
		fc.Advance(time.Second)
		testutil.Eventually(t, time.Second, func() bool { return quick.calls.Load() == want }, "quick run %d", want)
	}
	if n := slow.calls.Load(); n != 1 {
		t.Fatalf("slow ran %d times while stuck", n)
	}
}

func TestScheduler_TimeoutCancelsTheRunAndKeepsWhatWasEmitted(t *testing.T) {
	c := &fake{name: "hang", iv: time.Second, collect: func(ctx context.Context, emit Emit) error {
		emit(Metric{Name: "partial", Value: 1})
		<-ctx.Done()
		return ctx.Err()
	}}
	var logs syncBuffer
	fc, sk, reg := start(t, Options{Collectors: []Collector{c}, Timeout: 20 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "not armed")
	fc.Advance(time.Second)
	// Running: the ticker and the run's timeout, both on the fake clock.
	testutil.Eventually(t, time.Second, func() bool { return c.calls.Load() == 1 && fc.Waiters() == 2 }, "run not started")
	fc.Advance(19 * time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	if counter(reg, "ozy.agent.collector.timeouts", "hang") != 0 {
		t.Fatal("timed out before the limit")
	}
	fc.Advance(time.Millisecond)
	testutil.Eventually(t, 2*time.Second, func() bool { return counter(reg, "ozy.agent.collector.timeouts", "hang") == 1 }, "timeout not counted")
	testutil.Eventually(t, time.Second, func() bool { return len(sk.all()) == 1 }, "the partial batch was not sent")
	if n := counter(reg, "ozy.agent.collector.errors", "hang"); n != 1 {
		t.Fatalf("errors = %d, want 1", n)
	}
	// The collector returned context.Canceled; the report says what it was.
	if !strings.Contains(logs.String(), "timed out after 20ms: context deadline exceeded") {
		t.Errorf("log:\n%s", logs.String())
	}
}

// A run that returns before its limit is not a timeout, however close.
func TestScheduler_ARunJustInsideTheLimitIsNotATimeout(t *testing.T) {
	var fc *testutil.FakeClock
	c := &fake{name: "close", iv: time.Second, collect: func(context.Context, Emit) error {
		fc.Advance(999 * time.Millisecond) // the run takes 999ms of its 1s
		return nil
	}}
	var reg *selfmetrics.Registry
	fc, _, reg = start(t, Options{Collectors: []Collector{c}, Timeout: time.Second})
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "not armed")
	fc.Advance(time.Second)
	testutil.Eventually(t, time.Second, func() bool { return counter(reg, "ozy.agent.collector.runs", "close") == 1 }, "ran")
	if n := counter(reg, "ozy.agent.collector.timeouts", "close"); n != 0 {
		t.Fatalf("timeouts = %d for a run inside its limit", n)
	}
}

// A check pointed at something that is down fails every run. It is counted
// every run but logged once, and once more when it recovers.
func TestScheduler_LogsAFailureOnceAndTheRecovery(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	c := &fake{name: "flaky", iv: time.Second, collect: func(context.Context, Emit) error {
		if fail.Load() {
			return errors.New("connection refused")
		}
		return nil
	}}
	var buf syncBuffer
	fc, _, reg := start(t, Options{Collectors: []Collector{c}, Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	step := func(want int32) {
		testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "armed")
		fc.Advance(time.Second)
		testutil.Eventually(t, time.Second, func() bool { return c.calls.Load() == want }, "run %d", want)
	}
	for i := int32(1); i <= 3; i++ {
		step(i)
	}
	fail.Store(false)
	step(4)
	step(5)
	testutil.Eventually(t, time.Second, func() bool { return strings.Contains(buf.String(), "recovered") }, "no recovery line")
	if n := strings.Count(buf.String(), "collector failed"); n != 1 {
		t.Fatalf("logged the failure %d times:\n%s", n, buf.String())
	}
	if n := strings.Count(buf.String(), "recovered"); n != 1 {
		t.Fatalf("logged recovery %d times", n)
	}
	if n := counter(reg, "ozy.agent.collector.errors", "flaky"); n != 3 {
		t.Fatalf("errors = %d, want 3", n)
	}
}

func TestScheduler_DropsWhatCannotBeSent(t *testing.T) {
	leaked := make(chan Emit, 1)
	c := &fake{name: "bad", iv: time.Second, collect: func(_ context.Context, emit Emit) error {
		emit(Metric{Name: "ok", Value: 1, Tags: []string{"fine:yes", "k:a,b"}})
		emit(Metric{Name: "", Value: 1})
		emit(Metric{Name: "nan", Value: math.NaN()})
		emit(Metric{Name: "inf", Value: math.Inf(1)})
		emit(Metric{Name: "kind", Kind: Kind(9), Value: 1})
		leaked <- emit
		return nil
	}}
	fc, sk, reg := start(t, Options{Collectors: []Collector{c}})
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "armed")
	fc.Advance(time.Second)
	testutil.Eventually(t, time.Second, func() bool { return len(sk.all()) == 1 }, "batch")
	// Called after Collect returned: its batch has gone.
	(<-leaked)(Metric{Name: "late", Value: 1})
	if n := counter(reg, "ozy.agent.collector.dropped", "bad"); n != 5 {
		t.Fatalf("dropped = %d, want 5 (4 unsendable + 1 late)", n)
	}
	if n := counter(reg, "ozy.agent.collector.tags_dropped", "bad"); n != 1 {
		t.Fatalf("tags_dropped = %d, want 1", n)
	}
	if got := sk.all()[0]; got.Metric != "ok" || !slices.Equal(got.Tags, []string{"fine:yes"}) {
		t.Fatalf("sent %+v", got)
	}
}

func TestScheduler_Interval(t *testing.T) {
	s := New(Options{Interval: 20 * time.Second})
	for _, tc := range []struct{ in, want time.Duration }{
		{0, 20 * time.Second},
		{1500 * time.Millisecond, 2 * time.Second}, // whole seconds, rounded up
		{time.Millisecond, time.Second},
		{-time.Second, 20 * time.Second},
		{30 * time.Second, 30 * time.Second},
	} {
		if got := s.interval(&fake{iv: tc.in}); got != tc.want {
			t.Errorf("interval(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestScheduler_StopsCleanly(t *testing.T) {
	testutil.CheckGoroutines(t)
	block := &fake{name: "block", iv: time.Second, collect: func(ctx context.Context, _ Emit) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	idle := &fake{name: "idle", iv: time.Hour, collect: func(context.Context, Emit) error { return nil }}
	fc := testutil.NewFakeClock(t0)
	reg := selfmetrics.NewRegistry()
	s := New(Options{Collectors: []Collector{block, idle}, Clock: fc, Timeout: time.Hour, Registry: reg, Logger: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 2 }, "armed")
	fc.Advance(time.Second) // block starts and blocks; idle's jitter is still pending
	testutil.Eventually(t, time.Second, func() bool { return block.calls.Load() == 1 }, "block running")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return: a collector in Collect or waiting on jitter held it")
	}
	// The run shutdown cut short returned ctx's error; that is not a failure,
	// and counting it would chart an error at every restart.
	if n := counter(reg, "ozy.agent.collector.errors", "block"); n != 0 || counter(reg, "ozy.agent.collector.runs", "block") != 1 {
		t.Errorf("errors = %d after a run cancelled by shutdown, want 0", n)
	}
}

// syncBuffer is a bytes.Buffer safe for the logger's concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// A collector measuring another machine names its host; the agent's is not
// added alongside it.
func TestScheduler_ACollectorsOwnHostWins(t *testing.T) {
	c := &fake{name: "remote", iv: time.Second, collect: func(_ context.Context, emit Emit) error {
		emit(Metric{Name: "db.up", Value: 1, Tags: []string{"host:db1"}})
		emit(Metric{Name: "local.up", Value: 1})
		return nil
	}}
	fc, sk, _ := start(t, Options{Collectors: []Collector{c}, HostTag: "host:mac", Tags: []string{"env:dev"}})
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "armed")
	fc.Advance(time.Second)
	testutil.Eventually(t, time.Second, func() bool { return len(sk.all()) == 2 }, "batch")
	got := sk.all()
	if !slices.Equal(got[0].Tags, []string{"env:dev", "host:db1"}) {
		t.Errorf("db.up tags = %v, want host:db1 alone", got[0].Tags)
	}
	if !slices.Equal(got[1].Tags, []string{"env:dev", "host:mac"}) {
		t.Errorf("local.up tags = %v", got[1].Tags)
	}
}

// A collector stuck where cancellation cannot reach it (statfs on a hung
// disk) must not hold shutdown past the timeout.
func TestScheduler_AbandonsACollectorThatIgnoresCancellation(t *testing.T) {
	release := make(chan struct{}, 1)
	stuck := &fake{name: "stuck", iv: time.Second, collect: func(_ context.Context, emit Emit) error {
		<-release // ignores ctx
		emit(Metric{Name: "late", Value: 1})
		return nil
	}}
	fc := testutil.NewFakeClock(t0)
	var buf syncBuffer
	var sent atomic.Int32
	reg := selfmetrics.NewRegistry()
	s := New(Options{Collectors: []Collector{stuck}, Clock: fc, ShutdownTimeout: 2 * time.Second, Registry: reg,
		Sink:   func([]wire.Series) { sent.Add(1) },
		Logger: slog.New(slog.NewTextHandler(&buf, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "armed")
	fc.Advance(time.Second)
	testutil.Eventually(t, time.Second, func() bool { return stuck.calls.Load() == 1 }, "running")
	cancel()
	// The bound is on the injected clock: nothing returns until it moves.
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() >= 1 }, "shutdown timer")
	select {
	case <-done:
		t.Fatal("Run returned before its shutdown bound")
	case <-time.After(20 * time.Millisecond):
	}
	fc.Advance(2 * time.Second)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run waited for a collector that ignores cancellation")
	}
	if !strings.Contains(buf.String(), "abandoning") || !strings.Contains(buf.String(), "stuck") {
		t.Errorf("abandonment not logged: %s", buf.String())
	}
	// When the stuck collector finally returns, its batch is dropped, not
	// handed to a sink that is shutting down.
	release <- struct{}{}
	testutil.Eventually(t, time.Second, func() bool { return counter(reg, "ozy.agent.collector.dropped", "stuck") == 1 }, "late batch not dropped")
	if n := sent.Load(); n != 0 {
		t.Fatalf("sink called %d times after Run returned", n)
	}
}

// Autodiscovery's path: a collector added while Run runs is started, one
// removed stops, and a second of the same name runs beside it, sharing its
// self-metrics — a recreated container's check starts at once, and folded
// replicas are all checked. Once none of that name runs, its self-metrics
// are reported a last time and then forgotten.
func TestScheduler_AddAndRemoveWhileRunning(t *testing.T) {
	testutil.CheckGoroutines(t)
	emitting := func(name string) *fake {
		return &fake{name: name, iv: time.Second, collect: func(_ context.Context, emit Emit) error {
			emit(Metric{Name: name + ".up", Value: 1})
			return nil
		}}
	}
	fc := testutil.NewFakeClock(t0)
	sk := &sink{}
	reg := selfmetrics.NewRegistry()
	s := New(Options{Clock: fc, Sink: sk.send, Rand: rand.New(rand.NewPCG(1, 2)), Registry: reg, Logger: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	a := emitting("a")
	testutil.Eventually(t, time.Second, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.ctx != nil }, "Run started")
	removeA := s.Add(a)
	a2 := emitting("a")
	removeA2 := s.Add(a2) // same name: runs too
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 2 }, "both armed")
	fc.Advance(time.Second)
	testutil.Eventually(t, time.Second, func() bool { return a.calls.Load() == 1 && a2.calls.Load() == 1 }, "both ran")
	testutil.Eventually(t, time.Second, func() bool { return counter(reg, "ozy.agent.collector.runs", "a") == 2 }, "runs shared")

	removeA()
	testutil.Eventually(t, time.Second, func() bool { return len(s.Collectors()) == 1 }, "a stopped")
	fc.Advance(5 * time.Second)
	testutil.Eventually(t, time.Second, func() bool { return a2.calls.Load() >= 2 }, "a2 still runs")
	if n := a.calls.Load(); n != 1 {
		t.Fatalf("a ran %d times after removal", n-1)
	}
	rep := selfmetrics.NewReporter(reg, 10*time.Second)
	rep.Collect(t0)
	if !reported(rep.Collect(t0.Add(10*time.Second)), "collector:a") {
		t.Fatal("a2's self-metrics went with a")
	}
	removeA2()
	testutil.Eventually(t, time.Second, func() bool { return len(s.Collectors()) == 0 }, "a2 stopped")
	if !reported(rep.Collect(t0.Add(20*time.Second)), "collector:a") {
		t.Fatal("the last report of a's self-metrics was skipped")
	}
	if reported(rep.Collect(t0.Add(30*time.Second)), "collector:a") {
		t.Fatal("self-metrics of a name nothing runs under were kept")
	}
}

// reported says whether any series in out carries tag.
func reported(out []wire.Series, tag string) bool {
	for _, s := range out {
		if slices.Contains(s.Tags, tag) {
			return true
		}
	}
	return false
}

// Removing a collector that has not started yet (Run has not been called)
// takes it out of the queue.
func TestScheduler_RemoveBeforeRun(t *testing.T) {
	s := New(Options{Logger: slog.New(slog.DiscardHandler)})
	remove := s.Add(&fake{name: "x", collect: func(context.Context, Emit) error { return nil }})
	remove()
	if len(s.Collectors()) != 0 {
		t.Fatal("still scheduled")
	}
}

// A Distribution goes to the sketch sink, tagged and stamped like a series;
// an empty one is dropped, and so is one with no sink to go to.
func TestScheduler_Distributions(t *testing.T) {
	sk := sketch.NewDefault()
	_ = sk.Add(0.2)
	_ = sk.AddWithCount(1.5, 3)
	c := &fake{name: "hist", iv: time.Second, collect: func(_ context.Context, emit Emit) error {
		emit(Metric{Name: "req.latency", Kind: Distribution, Sketch: sk, Tags: []string{"route:/x"}})
		emit(Metric{Name: "empty", Kind: Distribution, Sketch: sketch.NewDefault()})
		emit(Metric{Name: "none", Kind: Distribution})
		return nil
	}}
	var mu sync.Mutex
	var got []wire.SketchSeries
	fc, _, reg := start(t, Options{Collectors: []Collector{c}, HostTag: "host:box", SketchSink: func(s []wire.SketchSeries) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, s...)
	}})
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "armed")
	fc.Advance(time.Second)
	testutil.Eventually(t, time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 }, "sketch sent")
	mu.Lock()
	s := got[0]
	mu.Unlock()
	if s.Metric != "req.latency" || s.Interval != 1 || len(s.Points) != 1 || s.Points[0].Timestamp != t0.Add(time.Second).Unix() ||
		!slices.Equal(s.Tags, []string{"host:box", "route:/x"}) || s.Points[0].Sketch.Count != 4 {
		t.Fatalf("got %+v", s)
	}
	if n := counter(reg, "ozy.agent.collector.dropped", "hist"); n != 2 {
		t.Errorf("dropped = %d, want the empty and the missing sketch", n)
	}

	// No sketch sink: counted as dropped, not lost silently.
	fc2, _, reg2 := start(t, Options{Collectors: []Collector{&fake{name: "h2", iv: time.Second, collect: func(_ context.Context, emit Emit) error {
		emit(Metric{Name: "x", Kind: Distribution, Sketch: sk})
		return nil
	}}}})
	testutil.Eventually(t, time.Second, func() bool { return fc2.Waiters() == 1 }, "armed")
	fc2.Advance(time.Second)
	testutil.Eventually(t, time.Second, func() bool { return counter(reg2, "ozy.agent.collector.dropped", "h2") == 1 }, "dropped")
}

// Review finding: a collector removed mid-run (its container went, its
// settings changed) or stopped by shutdown saw its context cancelled, and
// what it emitted then — a check's can_connect 0 for a cancelled dial — was
// sent as a reading. A stopped run sends nothing; it counts as dropped.
func TestScheduler_ARunCutShortByRemovalIsNotSent(t *testing.T) {
	testutil.CheckGoroutines(t)
	fc := testutil.NewFakeClock(t0)
	sk := &sink{}
	reg := selfmetrics.NewRegistry()
	s := New(Options{Clock: fc, Sink: sk.send, Rand: rand.New(rand.NewPCG(1, 2)), Registry: reg, Logger: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	testutil.Eventually(t, time.Second, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.ctx != nil }, "Run started")

	running := make(chan struct{})
	c := &fake{name: "dial", iv: time.Second, collect: func(ctx context.Context, emit Emit) error {
		close(running)
		<-ctx.Done()
		emit(Metric{Name: "x.can_connect", Value: 0}) // what a cancelled dial looks like
		return ctx.Err()
	}}
	remove := s.Add(c)
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "armed")
	fc.Advance(time.Second)
	<-running
	remove()
	testutil.Eventually(t, time.Second, func() bool { return len(s.Collectors()) == 0 }, "stopped")
	if got := sk.all(); len(got) != 0 {
		t.Fatalf("a run cut short by removal was sent: %+v", got)
	}
	if n := counter(reg, "ozy.agent.collector.dropped", "dial"); n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
	if n := counter(reg, "ozy.agent.collector.errors", "dial"); n != 0 {
		t.Fatalf("errors = %d: a stop is not a failure", n)
	}
}

// A metric's Keep tags reach the wire even when its own tags overflow the
// cap: they are what keeps two replicas' series apart.
func TestScheduler_KeepTagsSurviveTheCap(t *testing.T) {
	var own []string
	for i := range 60 {
		own = append(own, "label"+strconv.Itoa(i)+":v")
	}
	c := &fake{name: "scrape", iv: time.Second, collect: func(_ context.Context, emit Emit) error {
		emit(Metric{Name: "x.up", Value: 1, Tags: own, Keep: []string{"replica:1", "container_name:job"}})
		return nil
	}}
	fc, sk, reg := start(t, Options{Collectors: []Collector{c}})
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "armed")
	fc.Advance(time.Second)
	testutil.Eventually(t, time.Second, func() bool { return len(sk.all()) == 1 }, "sent")
	tags := sk.all()[0].Tags
	if len(tags) != wire.MaxTagsPerPoint || !slices.Contains(tags, "replica:1") || !slices.Contains(tags, "container_name:job") {
		t.Fatalf("%d tags, replica kept %v: %v", len(tags), slices.Contains(tags, "replica:1"), tags)
	}
	if n := counter(reg, "ozy.agent.collector.tags_dropped", "scrape"); n != 12 {
		t.Fatalf("tags_dropped = %d, want 12 (62 tags, 50 fit)", n)
	}
}

// A check that panics fails its run; the scheduler, and every other
// collector, carry on. Before, a panic in any parser ended the agent.
func TestScheduler_APanicFailsTheRunNotTheAgent(t *testing.T) {
	var logs syncBuffer
	bad := &fake{name: "bad", iv: time.Second, collect: func(context.Context, Emit) error { panic("index out of range") }}
	good := &fake{name: "good", iv: time.Second, collect: func(_ context.Context, emit Emit) error {
		emit(Metric{Name: "good.up", Value: 1})
		return nil
	}}
	fc, sk, reg := start(t, Options{Collectors: []Collector{bad, good}, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 2 }, "armed")
	fc.Advance(time.Second)
	testutil.Eventually(t, time.Second, func() bool { return counter(reg, "ozy.agent.collector.errors", "bad") == 1 }, "the panic was not a failed run")
	testutil.Eventually(t, time.Second, func() bool { return len(sk.all()) == 1 }, "the other collector's run")
	fc.Advance(time.Second)
	testutil.Eventually(t, time.Second, func() bool { return bad.calls.Load() == 2 }, "the panicking collector was not run again")
	if !strings.Contains(logs.String(), "panicked") || !strings.Contains(logs.String(), "index out of range") {
		t.Errorf("log:\n%s", logs.String())
	}
}

// Review finding: a second Run returned at its guard but still ran the
// deferred stop, so the first, live Run's sink was switched off and every
// batch after counted as dropped. A second call is a no-op.
func TestScheduler_ASecondRunDoesNotStopTheFirst(t *testing.T) {
	c := &fake{name: "c", iv: time.Second, collect: func(_ context.Context, emit Emit) error {
		emit(Metric{Name: "c.up", Value: 1})
		return nil
	}}
	fc := testutil.NewFakeClock(t0)
	sk := &sink{}
	s := New(Options{Collectors: []Collector{c}, Clock: fc, Sink: sk.send, Rand: rand.New(rand.NewPCG(1, 2)), Logger: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "armed")
	s.Run(context.Background()) // returns at once
	fc.Advance(time.Second)
	testutil.Eventually(t, time.Second, func() bool { return len(sk.all()) == 1 }, "the first Run's batch was not sent")
}

// Which collectors' tags are renamed on a clash is the scheduler's call, by
// what the collector is, not by whether it has keep tags. Round 7 inferred
// "built-in" from an empty Keep, and a lone unnamed check has one too.
func TestScheduler_OnlyAChecksClashingTagsAreExported(t *testing.T) {
	emits := func(tag string) func(context.Context, Emit) error {
		return func(_ context.Context, emit Emit) error {
			emit(Metric{Name: "m", Value: 1, Tags: []string{tag}})
			return nil
		}
	}
	builtIn := &fake{name: "docker", iv: time.Second, collect: emits("service:shop-api")}
	lone := &instance{name: "openmetrics", iv: time.Second, inner: &fake{name: "openmetrics", iv: time.Second, collect: emits("env:staging")}}
	fc, sk, _ := start(t, Options{Collectors: []Collector{builtIn, lone}, Tags: []string{"service:infra", "env:prod"}, HostTag: "host:mac"})
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 2 }, "armed")
	fc.Advance(time.Second)
	testutil.Eventually(t, time.Second, func() bool { return len(sk.all()) == 2 }, "both runs")
	for _, se := range sk.all() {
		switch {
		case slices.Contains(se.Tags, "service:infra") && slices.Contains(se.Tags, "env:prod") && slices.Contains(se.Tags, "service:shop-api"):
			// the built-in, decorated as before: both service values
		case slices.Contains(se.Tags, "exported_env:staging") && !slices.Contains(se.Tags, "env:staging"):
			// the lone check: its clashing tag exported
		default:
			t.Errorf("tags %v", se.Tags)
		}
	}
}

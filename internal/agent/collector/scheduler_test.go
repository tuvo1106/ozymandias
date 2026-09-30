package collector

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
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

func counter(reg *selfmetrics.Registry, name, coll string) int64 {
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
	slow := &fake{name: "slow", iv: time.Second, collect: func(ctx context.Context, _ Emit) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
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
	fc, sk, reg := start(t, Options{Collectors: []Collector{c}, Timeout: 20 * time.Millisecond})
	testutil.Eventually(t, time.Second, func() bool { return fc.Waiters() == 1 }, "not armed")
	fc.Advance(time.Second)
	testutil.Eventually(t, 2*time.Second, func() bool { return counter(reg, "ozy.agent.collector.timeouts", "hang") == 1 }, "timeout not counted")
	testutil.Eventually(t, time.Second, func() bool { return len(sk.all()) == 1 }, "the partial batch was not sent")
	if n := counter(reg, "ozy.agent.collector.errors", "hang"); n != 1 {
		t.Fatalf("errors = %d, want 1", n)
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
	s := New(Options{Collectors: []Collector{block, idle}, Clock: fc, Timeout: time.Hour, Logger: slog.New(slog.DiscardHandler)})
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

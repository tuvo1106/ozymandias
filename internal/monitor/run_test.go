package monitor

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/monitor/eval"
	"github.com/tuvo1106/ozymandias/internal/monitor/spec"
	"github.com/tuvo1106/ozymandias/internal/testutil"
)

// synthetic returns n groups with random values around the threshold, and can
// block every evaluation until released.
type synthetic struct {
	groups int
	gate   chan struct{} // nil: never block

	mu      sync.Mutex
	active  int
	maxSeen int
	calls   atomic.Int64
}

func (s *synthetic) Evaluate(ctx context.Context, c *spec.Compiled, now time.Time) ([]eval.Value, eval.Data, error) {
	s.calls.Add(1)
	s.mu.Lock()
	s.active++
	s.maxSeen = max(s.maxSeen, s.active)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
	}()
	if s.gate != nil {
		select {
		case <-s.gate:
		case <-ctx.Done():
			return nil, eval.Data{}, ctx.Err()
		}
	}
	out := make([]eval.Value, s.groups)
	for g := range out {
		key := fmt.Sprintf("g:%d", g)
		out[g] = eval.Value{Key: key, Group: spec.Group{"g": fmt.Sprint(g)}, Present: true,
			Value: rand.Float64() * 40, DataTs: now.Add(-time.Minute)}
	}
	return out, eval.Data{}, nil
}

func (s *synthetic) Fetch(context.Context, *spec.Compiled, time.Time, time.Time) (eval.Data, error) {
	return eval.Data{}, nil
}

func runScheduler(t *testing.T, s *Scheduler, clk *testutil.FakeClock) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	testutil.Eventually(t, 10*time.Second, func() bool { return clk.Waiters() >= 2 }, "scheduler armed")
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("scheduler did not stop")
		}
	}
}

// stepUntil advances clk a second at a time (only while gate allows, if
// given) until cond holds, failing after a minute of real time.
//
// Never one big jump: the scheduler reads Now, computes its next due time and
// then re-arms its timer relative to the clock *at that moment*. An Advance
// landing between the two pushes the deadline out by the whole jump, so a
// single 10s Advance could miss every tick (it did, in CI). One-second steps
// bound that overshoot to a second and keep going until the work happens.
func stepUntil(t *testing.T, clk *testutil.FakeClock, gate, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		if gate == nil || gate() {
			clk.Advance(time.Second)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met: %s", what)
}

func createN(t *testing.T, db *meta.DB, n int, interval int) {
	t.Helper()
	for k := range n {
		if _, err := db.CreateMonitor(context.Background(), meta.MonitorRow{UID: fmt.Sprintf("m%d", k), Name: fmt.Sprintf("m%d", k),
			Type: "metric", Query: "avg(last_5m):avg:x{*} by {g} > 20",
			Options: []byte(fmt.Sprintf(`{"evaluation_interval_s":%d,"thresholds":{"warning":10}}`, interval)),
			Message: "@cap", Enabled: true}, t0); err != nil {
			t.Fatal(err)
		}
	}
}

// TestManyMonitorsManyGroups is L6: 200 monitors x 50 groups for three
// intervals on the fake clock, under -race, with a goroutine leak check.
func TestManyMonitorsManyGroups(t *testing.T) {
	if testing.Short() {
		t.Skip("L6 scale test")
	}
	testutil.CheckGoroutines(t)
	h := newHarness(t)
	createN(t, h.db, 200, 30)
	syn := &synthetic{groups: 50}
	h.s = New(Options{Store: h.db, Evaluator: syn, Outbox: h.out, Clock: h.clk, Workers: 4})
	stop := runScheduler(t, h.s, h.clk)
	for k := 1; k <= 3; k++ {
		want := int64(200 * k)
		// Advancing only while the pool is idle keeps "skipped == 0" a
		// statement about the scheduler, not about the test outrunning it.
		stepUntil(t, h.clk, func() bool { return idle(h.s) },
			func() bool { return h.s.evaluations.Value() >= want && idle(h.s) }, fmt.Sprint("round ", k))
	}
	stop()
	if n := h.s.commitErrors.Value(); n != 0 {
		t.Errorf("%d commits failed under concurrent workers", n)
	}
	if h.s.skipped.Value() != 0 {
		t.Errorf("skipped %d evaluations with an idle pool", h.s.skipped.Value())
	}
	states, err := h.db.States(context.Background(), 0)
	if err != nil || len(states) != 200*50 {
		t.Fatalf("%d states: %v", len(states), err)
	}
	if syn.maxSeen > 4 {
		t.Errorf("%d evaluations ran at once with 4 workers", syn.maxSeen)
	}
}

// TestPoolBoundAndOverrunSkip: with every evaluation blocked, no more than
// Workers run at once, and a monitor due while still running skips the tick
// and counts it.
func TestPoolBoundAndOverrunSkip(t *testing.T) {
	testutil.CheckGoroutines(t)
	h := newHarness(t)
	createN(t, h.db, 6, 10)
	syn := &synthetic{groups: 1, gate: make(chan struct{})}
	h.s = New(Options{Store: h.db, Evaluator: syn, Clock: h.clk, Workers: 2})
	stop := runScheduler(t, h.s, h.clk)
	stepUntil(t, h.clk, nil, func() bool { return syn.calls.Load() == 2 }, "two running")
	// The two running monitors are due again: skipped, not queued behind
	// themselves. The four waiting in the queue are not re-queued either.
	stepUntil(t, h.clk, nil, func() bool { return h.s.skipped.Value() >= 6 }, "skips counted")
	if c := syn.calls.Load(); c != 2 {
		t.Fatalf("%d evaluations started with 2 workers blocked", c)
	}
	syn.mu.Lock()
	if syn.maxSeen != 2 {
		t.Errorf("max concurrent %d, want 2", syn.maxSeen)
	}
	syn.mu.Unlock()
	close(syn.gate)
	testutil.Eventually(t, 5*time.Second, func() bool { return syn.calls.Load() == 6 }, "queue drains")
	stop()
}

func TestReloadPicksUpNewMonitors(t *testing.T) {
	testutil.CheckGoroutines(t)
	h := newHarness(t)
	syn := &synthetic{groups: 1}
	h.s = New(Options{Store: h.db, Evaluator: syn, Clock: h.clk})
	stop := runScheduler(t, h.s, h.clk)
	defer stop()
	createN(t, h.db, 1, 10)
	h.s.Reload()
	h.s.Reload() // coalesced, never blocks
	testutil.Eventually(t, 5*time.Second, func() bool { return h.s.Health().Monitors == 1 }, "reloaded")
	stepUntil(t, h.clk, nil, func() bool { return syn.calls.Load() >= 1 }, "evaluated")
}

func TestWatchdog(t *testing.T) {
	testutil.CheckGoroutines(t)
	h := newHarness(t)
	createN(t, h.db, 1, 10)
	syn := &synthetic{groups: 1, gate: make(chan struct{})} // evaluations never finish
	h.s = New(Options{Store: h.db, Evaluator: syn, Clock: h.clk, Workers: 1})
	stop := runScheduler(t, h.s, h.clk)
	if h.s.Health().Stalled {
		t.Fatal("stalled at start")
	}
	h.clk.Advance(time.Minute)
	if h.s.Health().Stalled {
		t.Fatal("stalled after a minute")
	}
	h.clk.Advance(2 * time.Minute)
	testutil.Eventually(t, 5*time.Second, func() bool { return h.s.Health().Stalled }, "stall detected")
	if age := h.s.Health().LastEvalAge; age < 3*time.Minute {
		t.Errorf("age %v", age)
	}
	close(syn.gate)
	h.clk.Advance(10 * time.Second)
	testutil.Eventually(t, 5*time.Second, func() bool { return !h.s.Health().Stalled }, "recovered")
	stop()
	// No monitors, no stall.
	empty := New(Options{Store: newHarness(t).db, Evaluator: syn, Clock: h.clk})
	if empty.Health().Stalled {
		t.Error("empty scheduler stalled")
	}
}

func TestPrune(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	_ = h.db.AddEvent(ctx, meta.EventRow{Ts: t0.Add(-40 * 24 * time.Hour), Kind: "transition", MonitorID: 1})
	_ = h.db.AddEvent(ctx, meta.EventRow{Ts: t0, Kind: "transition", MonitorID: 1})
	h.s.prune(ctx)
	if evs, _ := h.db.Events(ctx, meta.EventQuery{}); len(evs) != 1 {
		t.Fatalf("prune left %d", len(evs))
	}
	keep := New(Options{Store: h.db, Evaluator: h.ev, Clock: h.clk, EventRetention: -1})
	_ = h.db.AddEvent(ctx, meta.EventRow{Ts: t0.Add(-400 * 24 * time.Hour), Kind: "transition", MonitorID: 1})
	keep.prune(ctx)
	if evs, _ := h.db.Events(ctx, meta.EventQuery{}); len(evs) != 2 {
		t.Fatalf("negative retention pruned: %d", len(evs))
	}
}

// idle reports whether no evaluation is running (evaluations counts an
// evaluation when its query returns, before its commit).
func idle(s *Scheduler) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.running {
			return false
		}
	}
	return true
}

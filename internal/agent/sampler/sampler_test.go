package sampler

import (
	"fmt"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

var t0 = time.Unix(1_790_000_000, 0)

func chunk(service, resource string, prio *float64, errored bool) []wire.Span {
	m := map[string]float64{wire.MetricTopLevel: 1}
	if prio != nil {
		m[wire.MetricSamplingPriority] = *prio
	}
	e := 0
	if errored {
		e = 1
	}
	return []wire.Span{{TraceID: "t", SpanID: "s", Service: service, Name: "http.request", Resource: resource, Error: e, Metrics: m, Meta: map[string]string{"env": "dev"}}}
}

func f(v float64) *float64 { return &v }

func TestDecide_PriorityKeepsAndUserDropOverridesEverything(t *testing.T) {
	s := New(Options{})
	if r := s.Decide(chunk("a", "r1", f(1), false), t0); r != Priority {
		t.Errorf("priority 1: %q", r)
	}
	if r := s.Decide(chunk("a", "r1", f(2), false), t0); r != Priority {
		t.Errorf("priority 2: %q", r)
	}
	// Dropped by the head sampler, not an error, not rare: dropped.
	if r := s.Decide(chunk("a", "r1", f(0), false), t0); r != Dropped {
		t.Errorf("priority 0 on a seen route: %q", r)
	}
	// User drop beats an error and a first-seen resource.
	if r := s.Decide(chunk("a", "brand-new", f(-1), true), t0); r != Dropped {
		t.Errorf("user drop was overridden: %q", r)
	}
}

func TestDecide_ErrorsAreKeptThroughATokenBucket(t *testing.T) {
	s := New(Options{ErrorTPS: 10})
	s.Decide(chunk("a", "r", f(0), false), t0) // mark the resource as seen, so rare is not what keeps the errors
	kept := 0
	for i := 0; i < 100; i++ {
		if s.Decide(chunk("a", "r", f(0), true), t0) == Error {
			kept++
		}
	}
	if kept != 10 {
		t.Fatalf("kept %d errors in one instant, want the bucket's 10", kept)
	}
	// One second later the bucket has refilled.
	kept = 0
	for i := 0; i < 100; i++ {
		if s.Decide(chunk("a", "r", f(0), true), t0.Add(time.Second)) == Error {
			kept++
		}
	}
	if kept != 10 {
		t.Fatalf("kept %d after a second, want 10", kept)
	}
	// Half a second refills half.
	kept = 0
	for i := 0; i < 100; i++ {
		if s.Decide(chunk("a", "r", f(0), true), t0.Add(1500*time.Millisecond)) == Error {
			kept++
		}
	}
	if kept != 5 {
		t.Fatalf("kept %d after half a second, want 5", kept)
	}
}

func TestDecide_RareKeepsTheFirstOfEachResourcePerWindow(t *testing.T) {
	s := New(Options{RareTPS: 100})
	if r := s.Decide(chunk("a", "GET /x", f(0), false), t0); r != Rare {
		t.Fatalf("first sighting: %q", r)
	}
	if r := s.Decide(chunk("a", "GET /x", f(0), false), t0.Add(time.Minute)); r != Dropped {
		t.Errorf("second sighting within the window: %q", r)
	}
	if r := s.Decide(chunk("a", "GET /y", f(0), false), t0.Add(time.Minute)); r != Rare {
		t.Errorf("a different resource: %q", r)
	}
	if r := s.Decide(chunk("b", "GET /x", f(0), false), t0.Add(time.Minute)); r != Rare {
		t.Errorf("the same resource in another service: %q", r)
	}
	// Continuous traffic must not keep postponing "rare": the window is measured
	// from the last time it fired, not from the last sighting.
	for _, m := range []int{2, 4} { // steady traffic in between
		s.Decide(chunk("a", "GET /x", f(0), false), t0.Add(time.Duration(m)*time.Minute))
	}
	if r := s.Decide(chunk("a", "GET /x", f(0), false), t0.Add(5*time.Minute+time.Second)); r != Rare {
		t.Errorf("after the window: %q", r)
	}
}

func TestDecide_RareIsRateLimited(t *testing.T) {
	s := New(Options{RareTPS: 5})
	kept := 0
	for i := 0; i < 200; i++ {
		if s.Decide(chunk("a", fmt.Sprintf("r%d", i), f(0), false), t0) == Rare {
			kept++
		}
	}
	if kept != 5 {
		t.Errorf("kept %d first-seen resources in one instant, want 5", kept)
	}
}

func TestDecide_NonTopLevelSpansAreNotRareCandidates(t *testing.T) {
	s := New(Options{})
	c := chunk("a", "select 1", f(0), false)
	c[0].Metrics = map[string]float64{wire.MetricSamplingPriority: 0}
	if r := s.Decide(c, t0); r != Dropped {
		t.Errorf("%q", r)
	}
	if s.Decide(nil, t0) != Dropped {
		t.Error("empty chunk kept")
	}
}

func TestRates_TargetOverObservedCappedAtOne(t *testing.T) {
	s := New(Options{TargetTPS: 50, RateWindow: 10 * time.Second})
	// 1000 traces/s for one service, 5/s for another, over a 10s window.
	for sec := 0; sec < 10; sec++ {
		at := t0.Add(time.Duration(sec) * time.Second)
		for i := 0; i < 1000; i++ {
			s.Decide(chunk("busy", "r", f(0), false), at)
		}
		for i := 0; i < 5; i++ {
			s.Decide(chunk("quiet", "r", f(0), false), at)
		}
	}
	if len(s.Rates()) != 0 {
		t.Fatal("rates reported before a window closed")
	}
	s.Decide(chunk("busy", "r", f(0), false), t0.Add(10*time.Second)) // closes the window
	r := s.Rates()
	if got := r[RateKey("busy", "dev")]; got < 0.04 || got > 0.06 {
		t.Errorf("busy rate = %v, want about 0.05", got)
	}
	if got := r[RateKey("quiet", "dev")]; got != 1 {
		t.Errorf("quiet rate = %v, want 1", got)
	}
}

func TestRates_ReturnsACopy(t *testing.T) {
	s := New(Options{RateWindow: time.Second})
	s.Decide(chunk("a", "r", nil, false), t0)
	s.Decide(chunk("a", "r", nil, false), t0.Add(time.Second))
	r := s.Rates()
	r["x"] = 9
	if _, ok := s.Rates()["x"]; ok {
		t.Error("Rates exposes internal state")
	}
}

func TestRareMemoryIsBounded(t *testing.T) {
	s := New(Options{RareTPS: 1})
	for i := 0; i < maxRareEntries*2; i++ {
		s.Decide(chunk("a", fmt.Sprintf("r%d", i), f(0), false), t0)
	}
	s.mu.Lock()
	n := len(s.rareSeen)
	s.mu.Unlock()
	if n > maxRareEntries {
		t.Errorf("%d rare entries", n)
	}
}

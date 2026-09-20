package aggregator

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// t0 is a bucket boundary, so t0+k*10s is the start of bucket k.
var t0 = time.Unix(1_790_000_000, 0)

func at(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }

func newAgg(t testing.TB, opts Options) *Aggregator {
	t.Helper()
	if opts.Registry == nil {
		opts.Registry = selfmetrics.NewRegistry()
	}
	if opts.Rand == nil {
		opts.Rand = rand.New(rand.NewPCG(1, 2))
	}
	return New(opts)
}

// find returns the series named metric (and optionally with a tag).
func find(out []wire.Series, metric string) *wire.Series {
	for i := range out {
		if out[i].Metric == metric {
			return &out[i]
		}
	}
	return nil
}

func points(s *wire.Series) string {
	if s == nil {
		return "<none>"
	}
	parts := make([]string, len(s.Points))
	for i, p := range s.Points {
		parts[i] = fmt.Sprintf("%d:%g", p.Timestamp-t0.Unix(), p.Value)
	}
	return strings.Join(parts, " ")
}

func TestAggregator_CounterScalesBySampleRate(t *testing.T) {
	a := newAgg(t, Options{})
	a.Add(Sample{Name: "hits", Kind: Counter, Value: 1, SampleRate: 0.5}, at(1))
	a.Add(Sample{Name: "hits", Kind: Counter, Value: 2}, at(2))
	a.Add(Sample{Name: "hits", Kind: Counter, Value: 1, SampleRate: 0.1}, at(3))
	s := find(a.Flush(at(10), false), "hits")
	if s == nil || s.Type != wire.KindCount || s.Interval != 10 || points(s) != "0:14" {
		t.Fatalf("got %+v (%s), want count 0:14 (2+2+10)", s, points(s))
	}
}

func TestAggregator_InvalidSampleRateCountsAsOne(t *testing.T) {
	a := newAgg(t, Options{})
	a.Add(Sample{Name: "hits", Kind: Counter, Value: 1, SampleRate: 7}, at(1))
	if s := find(a.Flush(at(10), false), "hits"); points(s) != "0:1" {
		t.Fatalf("got %s", points(s))
	}
}

func TestAggregator_GaugeKeepsTheLastValueAndIsNotZeroFilled(t *testing.T) {
	a := newAgg(t, Options{})
	a.Add(Sample{Name: "depth", Kind: Gauge, Value: 3, SampleRate: 0.5}, at(1))
	a.Add(Sample{Name: "depth", Kind: Gauge, Value: 7}, at(5))
	s := find(a.Flush(at(10), false), "depth")
	if s == nil || s.Type != wire.KindGauge || s.Interval != 0 || points(s) != "0:7" {
		t.Fatalf("got %+v (%s)", s, points(s))
	}
	if out := a.Flush(at(20), false); len(out) != 0 {
		t.Fatalf("idle gauge emitted %v", out)
	}
}

func TestAggregator_SetCountsDistinctMembers(t *testing.T) {
	a := newAgg(t, Options{})
	for _, m := range []string{"u1", "u2", "u1", "u3", "u2"} {
		a.Add(Sample{Name: "users", Kind: Set, SetMember: m}, at(1))
	}
	if s := find(a.Flush(at(10), false), "users"); s == nil || s.Type != wire.KindGauge || points(s) != "0:3" {
		t.Fatalf("got %s", points(s))
	}
}

func TestAggregator_HistogramStats(t *testing.T) {
	for _, kind := range []Kind{Histogram, Distribution} {
		a := newAgg(t, Options{})
		for v := 1; v <= 100; v++ {
			a.Add(Sample{Name: "lat", Kind: kind, Value: float64(v), SampleRate: 0.5}, at(1))
		}
		out := a.Flush(at(10), false)
		for metric, want := range map[string]string{
			"lat.avg": "0:50.5", "lat.min": "0:1", "lat.max": "0:100",
			"lat.median": "0:50", "lat.95percentile": "0:95", "lat.count": "0:200",
		} {
			if got := points(find(out, metric)); got != want {
				t.Errorf("kind %d %s = %s, want %s", kind, metric, got, want)
			}
		}
		if c := find(out, "lat.count"); c.Type != wire.KindCount || c.Interval != 10 {
			t.Errorf("lat.count = %+v, want a count with interval 10", c)
		}
		if find(out, "lat.avg").Type != wire.KindGauge {
			t.Error("lat.avg is not a gauge")
		}
	}
}

// Beyond the cap, min/max/count/avg stay exact; the percentiles come from a
// uniform sample and stay close.
func TestAggregator_HistogramReservoirKeepsExactAggregates(t *testing.T) {
	a := newAgg(t, Options{HistogramMaxSamples: 100})
	for v := 1; v <= 10000; v++ {
		a.Add(Sample{Name: "lat", Kind: Histogram, Value: float64(v)}, at(1))
	}
	out := a.Flush(at(10), false)
	for metric, want := range map[string]string{"lat.min": "0:1", "lat.max": "0:10000", "lat.count": "0:10000", "lat.avg": "0:5000.5"} {
		if got := points(find(out, metric)); got != want {
			t.Errorf("%s = %s, want %s", metric, got, want)
		}
	}
	med := find(out, "lat.median").Points[0].Value
	if med < 3500 || med > 6500 {
		t.Errorf("median estimate %v is far from 5000", med)
	}
}

func TestAggregator_BucketBoundaries(t *testing.T) {
	a := newAgg(t, Options{})
	a.Add(Sample{Name: "hits", Kind: Counter, Value: 1}, at(9.999))
	a.Add(Sample{Name: "hits", Kind: Counter, Value: 10}, at(10))
	// At 19.9 only the first bucket has closed.
	if got := points(find(a.Flush(at(19.9), false), "hits")); got != "0:1" {
		t.Fatalf("first flush %s, want 0:1", got)
	}
	if got := points(find(a.Flush(at(20), false), "hits")); got != "10:10" {
		t.Fatalf("second flush %s, want 10:10", got)
	}
}

func TestAggregator_CounterZeroFillThenExpiry(t *testing.T) {
	a := newAgg(t, Options{ContextExpiry: 30 * time.Second})
	a.Add(Sample{Name: "errors", Kind: Counter, Value: 2}, at(1))
	if got := points(find(a.Flush(at(20), false), "errors")); got != "0:2 10:0" {
		t.Fatalf("got %s", got)
	}
	if a.Contexts() != 1 {
		t.Fatalf("contexts = %d, want 1 (idle 19s, expiry 30s)", a.Contexts())
	}
	// Zeros continue through the bucket 30s after the last data (0), then stop.
	if got := points(find(a.Flush(at(40), false), "errors")); got != "20:0 30:0" {
		t.Fatalf("got %s", got)
	}
	if out := a.Flush(at(60), false); len(out) != 0 {
		t.Fatalf("zero-filled past the expiry: %s", points(find(out, "errors")))
	}
	if a.Contexts() != 0 {
		t.Fatalf("contexts = %d, want 0 after expiry", a.Contexts())
	}
}

// A counter that goes quiet past the expiry and comes back before its
// context is collected must not emit thousands of zeros, and must emit its
// new data.
func TestAggregator_CounterLongGapJumpsToNextData(t *testing.T) {
	a := newAgg(t, Options{ContextExpiry: 20 * time.Second})
	a.Add(Sample{Name: "c", Kind: Counter, Value: 1}, at(1))
	// Never flushed in between; data again much later.
	a.Add(Sample{Name: "c", Kind: Counter, Value: 5}, at(10_001))
	got := points(find(a.Flush(at(10_010), false), "c"))
	if got != "0:1 10:0 20:0 10000:5" {
		t.Fatalf("got %s", got)
	}
}

func TestAggregator_NonCounterContextExpires(t *testing.T) {
	a := newAgg(t, Options{ContextExpiry: 30 * time.Second})
	a.Add(Sample{Name: "g", Kind: Gauge, Value: 1}, at(1))
	a.Flush(at(10), false)
	a.Flush(at(20), false)
	if a.Contexts() != 1 {
		t.Fatal("gauge context forgotten before expiry")
	}
	a.Flush(at(40), false)
	if a.Contexts() != 0 {
		t.Fatal("gauge context kept past expiry")
	}
}

func TestAggregator_LateSampleCountsInTheOldestOpenBucket(t *testing.T) {
	reg := selfmetrics.NewRegistry()
	a := newAgg(t, Options{Registry: reg})
	a.Add(Sample{Name: "c", Kind: Counter, Value: 1}, at(1))
	a.Flush(at(10), false) // bucket 0 emitted
	// Claims t=5 (within tolerance of now=12), but bucket 0 is gone.
	a.Add(Sample{Name: "c", Kind: Counter, Value: 4, Timestamp: at(5).Unix()}, at(12))
	if got := points(find(a.Flush(at(20), false), "c")); got != "10:4" {
		t.Fatalf("got %s, want the late 4 in bucket 10", got)
	}
	if reg.Counter("ozy.agent.aggregator.late_samples").Value() != 1 {
		t.Fatal("late sample not counted")
	}
}

func TestAggregator_ClientTimestampUsedOnlyWithinTolerance(t *testing.T) {
	a := newAgg(t, Options{})
	a.Add(Sample{Name: "g", Kind: Gauge, Value: 1, Timestamp: at(55).Unix()}, at(1))    // 54s ahead: trusted
	a.Add(Sample{Name: "h", Kind: Gauge, Value: 1, Timestamp: at(200).Unix()}, at(1))   // 199s ahead: ignored
	a.Add(Sample{Name: "i", Kind: Gauge, Value: 1, Timestamp: at(-300).Unix()}, at(20)) // too old: ignored
	out := a.Flush(at(100), false)
	if got := points(find(out, "g")); got != "50:1" {
		t.Errorf("g = %s, want 50:1", got)
	}
	if got := points(find(out, "h")); got != "0:1" {
		t.Errorf("h = %s, want 0:1 (receive time)", got)
	}
	if got := points(find(out, "i")); got != "20:1" {
		t.Errorf("i = %s, want 20:1 (receive time)", got)
	}
}

func TestAggregator_TagsNormalizedWithHostAndAgentTags(t *testing.T) {
	reg := selfmetrics.NewRegistry()
	a := newAgg(t, Options{Registry: reg, Hostname: "Mac.Local", Tags: []string{"env:dev", "1bad"}})
	a.Add(Sample{Name: "c", Kind: Counter, Value: 1, Tags: []string{"Route:/X", "b", "b", "9nope"}}, at(1))
	a.Add(Sample{Name: "d", Kind: Counter, Value: 1, Tags: []string{"host:other"}}, at(1))
	out := a.Flush(at(10), false)
	if got := strings.Join(find(out, "c").Tags, ","); got != "b,env:dev,host:mac.local,route:/x" {
		t.Errorf("c tags = %s", got)
	}
	if got := strings.Join(find(out, "d").Tags, ","); got != "env:dev,host:other" {
		t.Errorf("d tags = %s (a sample's own host must win)", got)
	}
	if n := reg.Counter("ozy.agent.aggregator.tags_dropped").Value(); n != 1 {
		t.Errorf("tags_dropped = %d, want 1 (9nope; 1bad was an agent tag, dropped at startup)", n)
	}
}

func TestAggregator_TagOrderDoesNotChangeTheContext(t *testing.T) {
	a := newAgg(t, Options{})
	a.Add(Sample{Name: "c", Kind: Counter, Value: 1, Tags: []string{"a:1", "b:2"}}, at(1))
	a.Add(Sample{Name: "c", Kind: Counter, Value: 1, Tags: []string{"b:2", "a:1", "a:1"}}, at(1))
	if a.Contexts() != 1 {
		t.Fatalf("contexts = %d, want 1", a.Contexts())
	}
}

func TestAggregator_TooManyTagsAreCutDeterministically(t *testing.T) {
	a := newAgg(t, Options{})
	var tags []string
	for i := range 60 {
		tags = append(tags, fmt.Sprintf("k%02d", i))
	}
	a.Add(Sample{Name: "c", Kind: Gauge, Value: 1, Tags: tags}, at(1))
	s := find(a.Flush(at(10), false), "c")
	if len(s.Tags) != wire.MaxTagsPerPoint || s.Tags[0] != "k00" || s.Tags[49] != "k49" {
		t.Fatalf("tags = %v", s.Tags)
	}
}

func TestAggregator_InvalidNamesAndKindsAreDropped(t *testing.T) {
	reg := selfmetrics.NewRegistry()
	a := newAgg(t, Options{Registry: reg})
	a.Add(Sample{Name: "1bad", Kind: Counter, Value: 1}, at(1))
	a.Add(Sample{Name: "ok", Kind: 0, Value: 1}, at(1))
	a.Add(Sample{Name: "bad-name", Kind: Counter, Value: 1}, at(1)) // repaired, kept
	out := a.Flush(at(10), false)
	if len(out) != 1 || out[0].Metric != "bad_name" {
		t.Fatalf("out = %v", out)
	}
	if n := reg.Counter("ozy.agent.aggregator.samples_dropped").Value(); n != 2 {
		t.Fatalf("samples_dropped = %d", n)
	}
}

func TestAggregator_SameNameDifferentKindsAreSeparate(t *testing.T) {
	a := newAgg(t, Options{})
	a.Add(Sample{Name: "x", Kind: Counter, Value: 1}, at(1))
	a.Add(Sample{Name: "x", Kind: Gauge, Value: 5}, at(1))
	if a.Contexts() != 2 {
		t.Fatalf("contexts = %d", a.Contexts())
	}
}

func TestAggregator_FinalFlushEmitsOpenBuckets(t *testing.T) {
	a := newAgg(t, Options{})
	a.Add(Sample{Name: "c", Kind: Counter, Value: 3}, at(12))
	a.Add(Sample{Name: "g", Kind: Gauge, Value: 4}, at(13))
	a.Add(Sample{Name: "h", Kind: Histogram, Value: 5}, at(14))
	out := a.Flush(at(15), true)
	if points(find(out, "c")) != "10:3" || points(find(out, "g")) != "10:4" || points(find(out, "h.max")) != "10:5" {
		t.Fatalf("final flush: c=%s g=%s h.max=%s", points(find(out, "c")), points(find(out, "g")), points(find(out, "h.max")))
	}
}

func TestAggregator_RunFlushesAtBoundariesAndOnShutdown(t *testing.T) {
	defer testutil.CheckGoroutines(t)
	clk := testutil.NewFakeClock(at(1))
	a := newAgg(t, Options{Clock: clk})
	var mu sync.Mutex
	var got []string
	sink := func(out []wire.Series) {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range out {
			got = append(got, s.Metric+"@"+points(&s))
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx, sink); close(done) }()
	testutil.Eventually(t, time.Second, func() bool { return clk.Waiters() == 1 }, "ticker not started")

	a.Add(Sample{Name: "c", Kind: Counter, Value: 1}, clk.Now())
	clk.Advance(5 * time.Second) // t=6: same bucket, nothing to flush
	clk.Advance(5 * time.Second) // t=11: bucket 0 closes
	testutil.Eventually(t, time.Second, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 }, "no flush at the boundary")
	a.Add(Sample{Name: "g", Kind: Gauge, Value: 2}, clk.Now())
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, " ") != "c@0:1 g@10:2" {
		t.Fatalf("got %q", got)
	}
}

// --- properties (L2) -------------------------------------------------------

// Whatever the arrival pattern — sample rates, client timestamps, late
// samples, flushes at arbitrary times — the flushed counter values sum to the
// sum of value/rate over the inputs. Nothing is lost or double counted.
func TestAggregator_CounterSumIsConserved(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := New(Options{Registry: selfmetrics.NewRegistry(), ContextExpiry: time.Minute})
		now := at(0)
		var want, got float64
		for range rapid.IntRange(1, 200).Draw(t, "n") {
			now = now.Add(time.Duration(rapid.IntRange(0, 15_000).Draw(t, "dt_ms")) * time.Millisecond)
			rate := rapid.SampledFrom([]float64{1, 0.5, 0.25, 0.1}).Draw(t, "rate")
			v := float64(rapid.IntRange(-5, 100).Draw(t, "v"))
			var ts int64
			if rapid.Bool().Draw(t, "hasTS") {
				ts = now.Unix() + int64(rapid.IntRange(-90, 90).Draw(t, "skew"))
			}
			tags := rapid.SliceOfN(rapid.SampledFrom([]string{"a:1", "b:2", "c"}), 0, 3).Draw(t, "tags")
			a.Add(Sample{Name: "c", Kind: Counter, Value: v, SampleRate: rate, Timestamp: ts, Tags: tags}, now)
			want += v / rate
			if rapid.Bool().Draw(t, "flush") {
				for _, s := range a.Flush(now, false) {
					for _, p := range s.Points {
						got += p.Value
					}
				}
			}
		}
		for _, s := range a.Flush(now.Add(time.Hour), true) {
			for _, p := range s.Points {
				got += p.Value
			}
		}
		if math.Abs(got-want) > 1e-6*math.Max(1, math.Abs(want)) {
			t.Fatalf("flushed Σ %v, want %v", got, want)
		}
	})
}

// Every emitted series' points are strictly increasing in time, and no
// (series, timestamp) pair is ever emitted twice across flushes — the store
// would treat a repeat as an overwrite.
func TestAggregator_NeverEmitsATimestampTwice(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := New(Options{Registry: selfmetrics.NewRegistry(), ContextExpiry: 30 * time.Second})
		now := at(0)
		seen := map[string]bool{}
		check := func(out []wire.Series) {
			for _, s := range out {
				for i, p := range s.Points {
					k := fmt.Sprintf("%s|%s|%d", s.Metric, strings.Join(s.Tags, ","), p.Timestamp)
					if seen[k] {
						t.Fatalf("emitted %s twice", k)
					}
					seen[k] = true
					if i > 0 && p.Timestamp <= s.Points[i-1].Timestamp {
						t.Fatalf("%s points out of order", s.Metric)
					}
				}
			}
		}
		for range rapid.IntRange(1, 100).Draw(t, "n") {
			now = now.Add(time.Duration(rapid.IntRange(0, 25_000).Draw(t, "dt_ms")) * time.Millisecond)
			kind := rapid.SampledFrom([]Kind{Counter, Gauge, Set, Histogram}).Draw(t, "kind")
			var ts int64
			if rapid.Bool().Draw(t, "hasTS") {
				ts = now.Unix() + int64(rapid.IntRange(-60, 60).Draw(t, "skew"))
			}
			// One name per kind: a counter and a gauge sharing a name are two
			// contexts that collide downstream by design (intake rejects the
			// second type), which is not what this property is about.
			a.Add(Sample{Name: fmt.Sprintf("m%d", kind), Kind: kind, Value: 1, SetMember: "x", Timestamp: ts}, now)
			if rapid.Bool().Draw(t, "flush") {
				check(a.Flush(now, false))
			}
		}
		check(a.Flush(now.Add(time.Hour), false))
	})
}

// --- concurrency (L6) ------------------------------------------------------

func TestAggregator_ConcurrentAddsLoseNothing(t *testing.T) {
	a := newAgg(t, Options{})
	const writers, perWriter = 8, 20000
	var wg sync.WaitGroup
	stop := make(chan struct{})
	var flushed float64
	var fmu sync.Mutex
	go func() { // flush concurrently with the writers
		now := at(0)
		for {
			select {
			case <-stop:
				return
			default:
				for _, s := range a.Flush(now, false) {
					fmu.Lock()
					for _, p := range s.Points {
						flushed += p.Value
					}
					fmu.Unlock()
				}
			}
		}
	}()
	for w := range writers {
		wg.Go(func() {
			tags := []string{fmt.Sprintf("w:%d", w%3)}
			for range perWriter {
				a.Add(Sample{Name: "c", Kind: Counter, Value: 1, Tags: tags}, at(1))
			}
		})
	}
	wg.Wait()
	close(stop)
	fmu.Lock()
	defer fmu.Unlock()
	for _, s := range a.Flush(at(100), true) {
		for _, p := range s.Points {
			flushed += p.Value
		}
	}
	if flushed != writers*perWriter {
		t.Fatalf("flushed Σ %v, want %d", flushed, writers*perWriter)
	}
}

func TestPercentileAndFloor(t *testing.T) {
	if !math.IsNaN(percentile(nil, 0.5)) {
		t.Error("percentile of nothing should be NaN")
	}
	if percentile([]float64{7}, 0) != 7 {
		t.Error("p0 of one value")
	}
	for _, tc := range [][3]int64{{15, 10, 10}, {-5, 10, -10}, {-10, 10, -10}, {0, 10, 0}} {
		if got := floorTo(tc[0], tc[1]); got != tc[2] {
			t.Errorf("floorTo(%d,%d) = %d, want %d", tc[0], tc[1], got, tc[2])
		}
	}
	if abs(-3) != 3 || abs(3) != 3 {
		t.Error("abs")
	}
}

// --- benchmarks (L12) ------------------------------------------------------

func BenchmarkAggregator_AddExistingContext(b *testing.B) {
	a := newAgg(b, Options{Hostname: "h", Tags: []string{"env:dev"}})
	tags := []string{"service:shop", "route:/api/comics", "method:get", "status:200"}
	now := at(1)
	b.ReportAllocs()
	for b.Loop() {
		a.Add(Sample{Name: "http.request.count", Kind: Counter, Value: 1, Tags: tags}, now)
	}
}

func BenchmarkAggregator_AddParallel(b *testing.B) {
	a := newAgg(b, Options{Hostname: "h"})
	now := at(1)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			i++
			tags := []string{"route:/r" + string(rune('a'+i%16))}
			a.Add(Sample{Name: "http.request.count", Kind: Counter, Value: 1, Tags: tags}, now)
		}
	})
}

var _ = slices.Sort[[]int]

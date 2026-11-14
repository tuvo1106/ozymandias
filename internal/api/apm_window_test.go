package api

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/internal/testutil"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/naive"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// oneSketch is a SketchReader holding one sketch at one time, for every series.
type oneSketch struct {
	at int64 // unix ms
	s  *sketch.Sketch
}

func (o oneSketch) ReadEach(_ context.Context, _ tsdb.SeriesRef, fromMs, toMs int64, fn func(sketchstore.Point) error) error {
	if o.at < fromMs || o.at > toMs {
		return nil
	}
	return fn(sketchstore.Point{TimeMs: o.at, Sketch: o.s})
}

// TestServices_NumbersAreTheWindowAndTheSparklineIsWhole: a row's requests
// count only the window, while its sparkline draws whole buckets like any
// chart. Traffic just before the window, in the sparkline's first bucket, is
// in the sparkline and not in the count; clamping the sparkline instead would
// cut its first bucket short, a dip that is not there.
func TestServices_NumbersAreTheWindowAndTheSparklineIsWhole(t *testing.T) {
	dir := t.TempDir()
	store, err := naive.Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	md, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })
	ctx := context.Background()
	const t0 = int64(1_790_000_000) // a multiple of 1000: the sparkline's buckets start here
	if err := md.Observe(ctx, "trace.http.request.hits", wire.KindCount, 10, now); err != nil {
		t.Fatal(err)
	}
	ref := tsdb.NewSeriesRef("trace.http.request.hits", []string{"service:shop", "env:e2e"})
	if _, err := store.Append(ctx, []tsdb.SeriesSamples{{Series: ref, Samples: []tsdb.Sample{
		{T: (t0 + 100) * 1000, V: 100}, // before the window, in its first sparkline bucket
		{T: (t0 + 500) * 1000, V: 10},  // inside the window
	}}}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&APM{Store: store, Types: md, Clock: testutil.NewFakeClock(now)}).Register(mux)

	// 30000 s: 1000 s sparkline buckets, the first [t0, t0+1000).
	from, to := t0+300, t0+300+30000
	_, body := call(mux, fmt.Sprintf("/api/v1/services?from=%d&to=%d", from*1000, to*1000))
	rows, _ := body["services"].([]any)
	if len(rows) != 1 {
		t.Fatalf("services: %v", body)
	}
	r := rows[0].(map[string]any)
	if r["requests"] != 10.0 {
		t.Errorf("requests = %v, want 10: only the window counts", r["requests"])
	}
	spark, _ := r["sparkline"].([]any)
	if len(spark) == 0 || spark[0] != 110.0 {
		t.Errorf("sparkline starts %v, want the whole first bucket (110)", spark)
	}
}

// TestServices_LatencyWithoutRequestsMakesNoRow: the latency sketches travel on
// their own hop and can arrive before the counts. A window with sketches but
// no counted request has no row, rather than one with 0 requests and a p95:
// the symptom the window clamp was found by.
func TestServices_LatencyWithoutRequestsMakesNoRow(t *testing.T) {
	dir := t.TempDir()
	store, err := naive.Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	md, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })
	ctx := context.Background()
	const t0 = int64(1_790_000_000)
	for name, kind := range map[string]wire.Kind{"trace.http.request.hits": wire.KindCount, "trace.http.request.duration": wire.KindDistribution} {
		if err := md.Observe(ctx, name, kind, 10, now); err != nil {
			t.Fatal(err)
		}
	}
	tags := []string{"service:shop", "env:e2e"}
	if _, err := store.Append(ctx, []tsdb.SeriesSamples{
		// The service is known (its hits metric exists) but counted nothing in the window.
		{Series: tsdb.NewSeriesRef("trace.http.request.hits", tags), Samples: []tsdb.Sample{{T: (t0 + 100) * 1000, V: 5}}},
		{Series: tsdb.NewSeriesRef("trace.http.request.duration"+wire.SuffixCount, tags), Samples: []tsdb.Sample{{T: (t0 + 500) * 1000, V: 3}}},
	}); err != nil {
		t.Fatal(err)
	}
	sk := sketch.NewDefault()
	for _, v := range []float64{0.1, 0.2, 0.3} {
		if err := sk.Add(v); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	(&APM{Store: store, Types: md, Sketches: oneSketch{at: (t0 + 500) * 1000, s: sk}, Clock: testutil.NewFakeClock(now)}).Register(mux)
	_, body := call(mux, fmt.Sprintf("/api/v1/services?from=%d&to=%d", (t0+300)*1000, (t0+3900)*1000))
	if rows, _ := body["services"].([]any); len(rows) != 0 {
		t.Errorf("latency without requests made a row: %v", rows)
	}
}

// growingSketches answers each successive read of a series with one more
// bucket, as a store does when sketches land between two of a request's reads.
type growingSketches struct {
	calls   int
	buckets []sketchstore.Point
}

func (g *growingSketches) ReadEach(_ context.Context, _ tsdb.SeriesRef, _, _ int64, fn func(sketchstore.Point) error) error {
	g.calls++
	for _, p := range g.buckets[:min(g.calls, len(g.buckets))] {
		if err := fn(p); err != nil {
			return err
		}
	}
	return nil
}

// TestServices_PercentilesAreOneReadOfTheSketches: a service row's p50, p95 and
// p99 are three queries. If a bucket lands between them they describe different
// data: p50 of the early sketches beside a p99 of the later ones. The three
// must be answered from one read. (Found as a flake in the APM e2e: "p95 with
// no p50"; this is the same race with the timing removed.)
func TestServices_PercentilesAreOneReadOfTheSketches(t *testing.T) {
	dir := t.TempDir()
	store, err := naive.Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	md, err := meta.Open(filepath.Join(dir, "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })
	ctx := context.Background()
	const t0 = int64(1_790_000_000)
	for name, kind := range map[string]wire.Kind{"trace.http.request.hits": wire.KindCount, "trace.http.request.duration": wire.KindDistribution} {
		if err := md.Observe(ctx, name, kind, 10, now); err != nil {
			t.Fatal(err)
		}
	}
	tags := []string{"service:shop", "env:e2e"}
	if _, err := store.Append(ctx, []tsdb.SeriesSamples{{
		Series: tsdb.NewSeriesRef("trace.http.request.hits", tags), Samples: []tsdb.Sample{{T: (t0 + 500) * 1000, V: 4}},
	}, {
		// A percentile query selects on the sketch's .count series.
		Series:  tsdb.NewSeriesRef("trace.http.request.duration"+wire.SuffixCount, tags),
		Samples: []tsdb.Sample{{T: (t0 + 500) * 1000, V: 10}, {T: (t0 + 510) * 1000, V: 10}},
	}}); err != nil {
		t.Fatal(err)
	}
	fast, slow := sketch.NewDefault(), sketch.NewDefault()
	for range 10 {
		_ = fast.Add(0.010) // 10 ms
		_ = slow.Add(5.0)   // 5 s: a later bucket far from the first
	}
	g := &growingSketches{buckets: []sketchstore.Point{
		{TimeMs: (t0 + 500) * 1000, Sketch: fast}, {TimeMs: (t0 + 510) * 1000, Sketch: slow},
	}}
	mux := http.NewServeMux()
	(&APM{Store: store, Types: md, Sketches: g, Clock: testutil.NewFakeClock(now)}).Register(mux)
	_, body := call(mux, fmt.Sprintf("/api/v1/services?from=%d&to=%d", (t0+300)*1000, (t0+3900)*1000))
	rows, _ := body["services"].([]any)
	if len(rows) != 1 {
		t.Fatalf("services: %v", body)
	}
	r := rows[0].(map[string]any)
	p50, p95, p99 := r["p50_ms"], r["p95_ms"], r["p99_ms"]
	for name, v := range map[string]any{"p50": p50, "p95": p95, "p99": p99} {
		f, ok := v.(float64)
		if !ok {
			t.Fatalf("%s = %v, want a number", name, v)
		}
		if f > 50 { // every percentile from the first read: only the 10 ms sketch
			t.Errorf("%s = %v ms: it read a bucket the others did not see", name, f)
		}
	}
	if g.calls != 1 {
		t.Errorf("the sketch store was read %d times for one series, want 1", g.calls)
	}
}

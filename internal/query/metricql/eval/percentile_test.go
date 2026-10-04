package eval

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// memSketches is a SketchReader over a fixed set of points.
type memSketches struct {
	points map[string][]sketchstore.Point
	err    error
	// ignoreRange hands back every point regardless of the window, the way a
	// store whose range is coarser than the query's grid would.
	ignoreRange bool
}

func (m *memSketches) ReadEach(_ context.Context, ref tsdb.SeriesRef, fromMs, toMs int64, fn func(sketchstore.Point) error) error {
	if m.err != nil {
		return m.err
	}
	for _, p := range m.points[ref.Key()] {
		if !m.ignoreRange && (p.TimeMs < fromMs || p.TimeMs > toMs) {
			continue
		}
		if err := fn(p); err != nil {
			return err
		}
	}
	return nil
}

func sketchOf(t *testing.T, values ...float64) *sketch.Sketch {
	t.Helper()
	s := sketch.NewDefault()
	for _, v := range values {
		if err := s.Add(v); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// sketchEnv builds an evaluator whose `lat` metric is a distribution: the
// `.count` series in the TSDB is what selection runs against, and the sketches
// live beside it under the bare metric name.
func sketchEnv(t *testing.T, buckets map[string]map[int64]*sketch.Sketch) *Evaluator {
	t.Helper()
	var counts []tsdb.SeriesSamples
	points := map[string][]sketchstore.Point{}
	for tag, byTime := range buckets {
		tags := []string{tag}
		var samples []tsdb.Sample
		ref := tsdb.NewSeriesRef("lat", tags)
		for ts, s := range byTime {
			samples = append(samples, tsdb.Sample{T: ts * 1000, V: s.Count()})
			points[ref.Key()] = append(points[ref.Key()], sketchstore.Point{TimeMs: ts * 1000, Sketch: s})
		}
		slicesSortSamples(samples)
		counts = append(counts, series("lat"+wire.SuffixCount, tags, samples))
	}
	return &Evaluator{
		Store:    &memStore{series: counts},
		Sketches: &memSketches{points: points},
		Types:    types{"lat": wire.KindDistribution},
		Timeout:  -1,
	}
}

func slicesSortSamples(s []tsdb.Sample) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].T < s[j-1].T; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// The property that makes sketches worth storing: a percentile across hosts is
// the quantile of the merge, not the mean of the quantiles. Host a is fast and
// host b is slow; a p50 of the pair has to describe the pair.
func TestEval_PercentileMergesRatherThanAverages(t *testing.T) {
	fast := make([]float64, 100)
	for i := range fast {
		fast[i] = 1
	}
	slow := make([]float64, 100)
	for i := range slow {
		slow[i] = 1000
	}
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{
		"host:a": {0: sketchOf(t, fast...)},
		"host:b": {0: sketchOf(t, slow...)},
	})

	// Merged, the 200 values are 100 ones and 100 thousands, so the median is
	// at the boundary: 1, the last value of the lower half.
	res := run(t, e, "p50:lat{*}", 0, 59, 60)
	got := res.Series[0].Points[0].V
	if math.Abs(got-1) > 0.01 {
		t.Errorf("merged p50 = %v, want ~1", got)
	}
	// The mean of the two hosts' medians would be ~500.5 — a number that
	// describes neither host and no host.
	if math.Abs(got-500.5) < 100 {
		t.Errorf("p50 = %v, which looks like the mean of two medians", got)
	}
	// And p99 of the merge is up in the slow host's values.
	res = run(t, e, "p99:lat{*}", 0, 59, 60)
	if v := res.Series[0].Points[0].V; v < 900 {
		t.Errorf("merged p99 = %v, want ~1000", v)
	}
}

// Every percentile aggregator is a quantile of the same merged sketch, as text
// through the parser and the planner. p75 and p90 were only ever covered as
// constants in the quantile table, never run as queries.
func TestEval_EveryPercentileAggregatorAnswersItsQuantile(t *testing.T) {
	values := make([]float64, 100)
	for i := range values {
		values[i] = float64(i + 1)
	}
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{"host:a": {0: sketchOf(t, values...)}})
	for agg, want := range map[string]float64{"p50": 50, "p75": 75, "p90": 90, "p95": 95, "p99": 99} {
		res := run(t, e, agg+":lat{*}", 0, 59, 60)
		if len(res.Series) != 1 {
			t.Fatalf("%s: %d lines, want 1", agg, len(res.Series))
		}
		if got := res.Series[0].Points[0].V; math.Abs(got-want)/want > 0.02 {
			t.Errorf("%s = %v, want ~%v (the sketch's 1%% relative error, doubled)", agg, got, want)
		}
	}
}

// Sketches are merged per output bucket, so a bucket wider than the agent's
// flush has to combine several of them.
func TestEval_PercentileMergesAcrossBuckets(t *testing.T) {
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{
		"host:a": {
			0:  sketchOf(t, 1, 1, 1, 1),
			30: sketchOf(t, 100, 100, 100, 100),
		},
	})
	// Two 30s buckets keep them apart.
	if got := lines(run(t, e, "p99:lat{*}", 0, 59, 30)); len(got) != 1 ||
		!strings.HasPrefix(got[0], "*: ") || strings.Count(got[0], ",") != 1 {
		t.Fatalf("got %q, want two buckets", got)
	}
	// One 60s bucket has to merge them: the 99th percentile of eight values,
	// four of which are 100, is up at 100.
	res := run(t, e, "p99:lat{*}", 0, 59, 60)
	if v := res.Series[0].Points[0].V; v < 90 {
		t.Errorf("p99 over the merged bucket = %v, want ~100", v)
	}
}

func TestEval_PercentileGroupsAndFiltersLikeAnyQuery(t *testing.T) {
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{
		"route:/x": {0: sketchOf(t, 1, 2, 3)},
		"route:/y": {0: sketchOf(t, 1000, 2000, 3000)},
	})
	res := run(t, e, "p50:lat{*} by {route}", 0, 59, 60)
	if len(res.Series) != 2 {
		t.Fatalf("got %d lines, want one per route", len(res.Series))
	}
	if res.Series[0].Scope != "route:/x" || res.Series[1].Scope != "route:/y" {
		t.Errorf("scopes %q and %q", res.Series[0].Scope, res.Series[1].Scope)
	}
	if v := res.Series[0].Points[0].V; math.Abs(v-2) > 0.05 {
		t.Errorf("/x p50 = %v, want ~2", v)
	}
	// And a filter narrows it to one, through the same index and matchers as
	// any other query.
	res = run(t, e, "p50:lat{route:/y} by {route}", 0, 59, 60)
	if len(res.Series) != 1 || res.Series[0].Scope != "route:/y" {
		t.Errorf("got %q, want only /y", lines(res))
	}
}

// A bucket with no sketch is a gap, and a series with no sketches in the window
// draws no line at all — an empty line in a legend is worse than no line.
func TestEval_PercentileGapsAndEmptySeries(t *testing.T) {
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{
		"host:a": {0: sketchOf(t, 5)},
	})
	res := run(t, e, "p95:lat{*}", 0, 59, 10)
	if got := lines(res); len(got) != 1 || got[0] != "*: 5,_,_,_,_,_" {
		t.Errorf("got %q, want one value then gaps", got)
	}

	// The count series exists in the window but its sketches do not, which is
	// the transient the intake's write order creates and the shape retention
	// used to create. It must draw nothing rather than a line of nulls.
	e.Sketches = &memSketches{}
	res = run(t, e, "p95:lat{*}", 0, 59, 10)
	if len(res.Series) != 0 {
		t.Errorf("got %q, want no line when no sketch was read", lines(res))
	}
}

// Sketches of one metric built at different accuracies cannot be merged.
// Answering from whichever subset agreed would be the confident wrong answer
// the whole design exists to avoid.
func TestEval_PercentileRefusesIncompatibleSketches(t *testing.T) {
	coarse := sketch.New(0.05)
	if err := coarse.Add(1); err != nil {
		t.Fatal(err)
	}
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{
		"host:a": {0: sketchOf(t, 1)},
		"host:b": {0: coarse},
	})
	_, err := runErr(e, "p95:lat{*}", 0, 59, 60, nil)
	if err == nil || !strings.Contains(err.Error(), "different gamma") {
		t.Fatalf("got %v, want an incompatibility error", err)
	}
	// It is the stored data that disagrees, not the query. Classifying this as
	// ErrBadQuery would hand the caller a 400 and send them rewriting a query
	// that was never the problem; the fix belongs to whoever is running the
	// writer that used the other accuracy.
	if !errors.Is(err, ErrSketchesDisagree) {
		t.Errorf("%v does not classify as ErrSketchesDisagree", err)
	}
	if errors.Is(err, ErrBadQuery) {
		t.Errorf("%v classifies as the caller's mistake, which it is not", err)
	}
	// And the metric is named, because that is what an operator goes looking
	// with.
	if !strings.Contains(err.Error(), "lat") {
		t.Errorf("%v does not name the metric", err)
	}
}

// Merging must not write through into the store's own sketches: the next query
// would then see data the first one put there.
func TestEval_PercentileDoesNotMutateTheStore(t *testing.T) {
	a, b := sketchOf(t, 1), sketchOf(t, 1000)
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{
		"host:a": {0: a},
		"host:b": {0: b},
	})
	first := run(t, e, "p99:lat{*}", 0, 59, 60).Series[0].Points[0].V
	if a.Count() != 1 || b.Count() != 1 {
		t.Fatalf("the query grew the stored sketches to %v and %v", a.Count(), b.Count())
	}
	second := run(t, e, "p99:lat{*}", 0, 59, 60).Series[0].Points[0].V
	if first != second {
		t.Errorf("the same query answered %v then %v", first, second)
	}
}

// A percentile query still selects on <metric>.count, which is what makes the
// sketches reachable through the ordinary index (ADR-0015).
func TestEval_PercentileSelectsOnTheCountSeries(t *testing.T) {
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{"host:a": {0: sketchOf(t, 1)}})
	run(t, e, "p95:lat{*}", 0, 59, 60)
	sel := e.Store.(*memStore).selectors[0]
	if sel.Metric != "lat"+wire.SuffixCount {
		t.Errorf("selected %q, want the .count series", sel.Metric)
	}
}

// A percentile's buckets are merged sketches; there is no choice of method,
// because a merge is the only reduction that keeps the error bound. The
// milestone's own example writes `.rollup(max, 60)` on a p95, so the width is
// honoured and the discarded method is reported rather than dropped in
// silence.
func TestEval_RollupMethodOnAPercentileWarns(t *testing.T) {
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{
		"host:a": {0: sketchOf(t, 1, 2, 3)},
	})
	res, err := runErr(e, "p95:lat{*}.rollup(max, 30)", 0, 59, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Interval != 30 {
		t.Errorf("interval %d, want the rollup's 30 — the width is still meaningful", res.Interval)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "max") {
		t.Errorf("warnings %q, want one naming the discarded method", res.Warnings)
	}
	// A rollup that names no method has nothing to discard and nothing to say.
	res, err = runErr(e, "p95:lat{*}", 0, 59, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings %q on a query with no rollup", res.Warnings)
	}
}

// A sketch read from the store but rejected by the grid must not register a
// group: an all-null line in the legend is worse than no line.
func TestEval_SketchesOffTheGridDrawNothing(t *testing.T) {
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{
		"host:a": {0: sketchOf(t, 1)},
	})
	// Widen what the reader hands back to include a point outside the window,
	// the way a store with a coarser range than the grid would.
	reader := e.Sketches.(*memSketches)
	for key := range reader.points {
		reader.points[key] = append(reader.points[key],
			sketchstore.Point{TimeMs: 9_000_000, Sketch: sketchOf(t, 99)})
	}
	reader.ignoreRange = true

	res := run(t, e, "p95:lat{*}", 0, 59, 60)
	if len(res.Series) != 1 {
		t.Fatalf("got %d lines, want the one with a sketch in the window", len(res.Series))
	}
	if v := res.Series[0].Points[0].V; math.Abs(v-1) > 0.05 {
		t.Errorf("p95 = %v, want ~1: the off-grid sketch must not be merged", v)
	}

	// And a series whose *only* sketches are off the grid draws nothing.
	e2 := sketchEnv(t, map[string]map[int64]*sketch.Sketch{"host:a": {0: sketchOf(t, 1)}})
	r2 := e2.Sketches.(*memSketches)
	for key := range r2.points {
		r2.points[key] = []sketchstore.Point{{TimeMs: 9_000_000, Sketch: sketchOf(t, 99)}}
	}
	r2.ignoreRange = true
	if res := run(t, e2, "p95:lat{*}", 0, 59, 60); len(res.Series) != 0 {
		t.Errorf("got %q, want no line at all", lines(res))
	}
}

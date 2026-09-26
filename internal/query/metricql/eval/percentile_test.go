package eval

import (
	"context"
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
}

func (m *memSketches) ReadEach(_ context.Context, ref tsdb.SeriesRef, fromMs, toMs int64, fn func(sketchstore.Point) error) error {
	if m.err != nil {
		return m.err
	}
	for _, p := range m.points[ref.Key()] {
		if p.TimeMs < fromMs || p.TimeMs > toMs {
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
	if res.Series[0].Scope() != "route:/x" || res.Series[1].Scope() != "route:/y" {
		t.Errorf("scopes %q and %q", res.Series[0].Scope(), res.Series[1].Scope())
	}
	if v := res.Series[0].Points[0].V; math.Abs(v-2) > 0.05 {
		t.Errorf("/x p50 = %v, want ~2", v)
	}
	// And a filter narrows it to one, through the same index and matchers as
	// any other query.
	res = run(t, e, "p50:lat{route:/y} by {route}", 0, 59, 60)
	if len(res.Series) != 1 || res.Series[0].Scope() != "route:/y" {
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
	if _, err := runErr(e, "p95:lat{*}", 0, 59, 60, nil); err == nil ||
		!strings.Contains(err.Error(), "different gamma") {
		t.Errorf("got %v, want an incompatibility error", err)
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

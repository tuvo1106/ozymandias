package simple

import (
	"context"
	"errors"
	"math"
	"sort"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/sketchstore"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// memSketches is a SketchReader over in-memory points, keyed the way the real
// store is: by the series' canonical key.
type memSketches struct {
	byKey map[string][]sketchstore.Point
	err   error
}

func (m *memSketches) Read(_ context.Context, ref tsdb.SeriesRef, from, to int64) ([]sketchstore.Point, error) {
	if m.err != nil {
		return nil, m.err
	}
	var out []sketchstore.Point
	for _, p := range m.byKey[ref.Key()] {
		if p.TimeMs >= from && p.TimeMs <= to {
			out = append(out, p)
		}
	}
	return out, nil
}

// fixture builds the two stores the percentile path reads: the `.count`
// series it selects on, and the sketches it merges. They are written together
// here for the same reason the intake writes them together.
type fixture struct {
	store    *memStore
	sketches *memSketches
	all      map[string][]float64 // series key -> every value, for the oracle
}

func newFixture() *fixture {
	return &fixture{
		store:    &memStore{},
		sketches: &memSketches{byKey: map[string][]sketchstore.Point{}},
		all:      map[string][]float64{},
	}
}

// add files one bucket of observations for one series.
func (f *fixture) add(t *testing.T, metric string, tags []string, tsSec int64, values ...float64) {
	t.Helper()
	s := sketch.NewDefault()
	for _, v := range values {
		if err := s.Add(v); err != nil {
			t.Fatalf("Add(%v): %v", v, err)
		}
	}
	ref := tsdb.NewSeriesRef(metric, tags)
	f.sketches.byKey[ref.Key()] = append(f.sketches.byKey[ref.Key()],
		sketchstore.Point{TimeMs: tsSec * 1000, Sketch: s})

	counted := tsdb.NewSeriesRef(metric+wire.SuffixCount, tags)
	for i := range f.store.series {
		if f.store.series[i].Series.Key() == counted.Key() {
			f.store.series[i].Samples = append(f.store.series[i].Samples,
				tsdb.Sample{T: tsSec * 1000, V: float64(len(values))})
			f.all[ref.Key()] = append(f.all[ref.Key()], values...)
			return
		}
	}
	f.store.series = append(f.store.series, tsdb.SeriesSamples{
		Series:  counted,
		Samples: []tsdb.Sample{{T: tsSec * 1000, V: float64(len(values))}},
	})
	f.all[ref.Key()] = append(f.all[ref.Key()], values...)
}

func (f *fixture) run(t *testing.T, req Request) Result {
	t.Helper()
	req.Kind = wire.KindDistribution
	res, err := Run(context.Background(), f.store, f.sketches, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

// exactQuantile is the answer the sketch is allowed to be within alpha of:
// the rank convention is q*(n-1) over the sorted raw values, the same one
// the sketch uses.
func exactQuantile(values []float64, q float64) float64 {
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	i := int(q * float64(len(s)-1))
	return s[i]
}

// The acceptance criterion of M2, in one test: a percentile over a fleet is
// within the sketch's relative error of the exact answer computed from the
// raw inputs.
func TestPercentile_WithinAlphaOfExact(t *testing.T) {
	f := newFixture()
	// Two hosts with very different latencies, so averaging their p95s would
	// be visibly wrong and only a merge can be right.
	for i := 1; i <= 500; i++ {
		f.add(t, "lat", []string{"host:a", "route:/r"}, 1000, float64(i))
		f.add(t, "lat", []string{"host:b", "route:/r"}, 1000, float64(i)*10)
	}

	var everything []float64
	for _, vs := range f.all {
		everything = append(everything, vs...)
	}

	for _, agg := range []Agg{P50, P75, P90, P95, P99} {
		q, _ := quantileOf(agg)
		res := f.run(t, Request{Metric: "lat", Agg: agg, From: 1000, To: 1009, Interval: 10})
		if len(res.Series) != 1 || len(res.Series[0].Points) != 1 {
			t.Fatalf("%s: %+v", agg, res)
		}
		got := res.Series[0].Points[0].V
		want := exactQuantile(everything, q)
		if math.Abs(got-want) > sketch.DefaultAlpha*math.Abs(want) {
			t.Errorf("%s = %v, want %v within %v relative error", agg, got, want, sketch.DefaultAlpha)
		}
	}
}

// Merge first, quantile second. The mean of the groups' percentiles is a
// different number with no error bound, and this is the test that says the
// code does not compute it.
func TestPercentile_MergesRatherThanAverages(t *testing.T) {
	f := newFixture()
	// 900 fast requests on one host, 100 slow ones on another: the fleet p95
	// is the slow host's latency, which the mean of the two p95s misses by
	// half.
	for i := 0; i < 900; i++ {
		f.add(t, "lat", []string{"host:fast"}, 1000, 10)
	}
	for i := 0; i < 100; i++ {
		f.add(t, "lat", []string{"host:slow"}, 1000, 1000)
	}
	res := f.run(t, Request{Metric: "lat", Agg: P95, From: 1000, To: 1009, Interval: 10})
	if len(res.Series) != 1 {
		t.Fatalf("got %d series, want one merged group: %+v", len(res.Series), res)
	}
	got := res.Series[0].Points[0].V
	if math.Abs(got-1000) > 0.01*1000 {
		t.Errorf("fleet p95 = %v, want ~1000 (the mean of the two p95s would be ~505)", got)
	}
}

// Grouping splits the merge, not the answer: each group merges its own
// series and takes its own quantile.
func TestPercentile_GroupBy(t *testing.T) {
	f := newFixture()
	for i := 1; i <= 100; i++ {
		f.add(t, "lat", []string{"host:a", "route:/x"}, 1000, float64(i))
		f.add(t, "lat", []string{"host:b", "route:/x"}, 1000, float64(i))
		f.add(t, "lat", []string{"host:a", "route:/y"}, 1000, float64(i)*100)
	}
	res := f.run(t, Request{Metric: "lat", Agg: P50, By: []string{"route"}, From: 1000, To: 1009, Interval: 10})
	if len(res.Series) != 2 {
		t.Fatalf("got %d groups, want 2: %+v", len(res.Series), res)
	}
	byRoute := map[string]float64{}
	for _, s := range res.Series {
		byRoute[s.Tags["route"]] = s.Points[0].V
	}
	if v := byRoute["/x"]; math.Abs(v-50) > 1 {
		t.Errorf("p50 of /x = %v, want ~50", v)
	}
	if v := byRoute["/y"]; math.Abs(v-5000) > 60 {
		t.Errorf("p50 of /y = %v, want ~5000", v)
	}
}

// Several agent buckets inside one output bucket merge into one answer;
// an output bucket with nothing in it is a gap, not a zero.
func TestPercentile_BucketsAndGaps(t *testing.T) {
	f := newFixture()
	f.add(t, "lat", nil, 1000, 1, 2, 3)
	f.add(t, "lat", nil, 1010, 100, 200, 300)
	// Nothing at 1020.
	f.add(t, "lat", nil, 1030, 7)

	res := f.run(t, Request{Metric: "lat", Agg: P99, From: 1000, To: 1039, Interval: 20})
	if len(res.Series) != 1 {
		t.Fatalf("%+v", res)
	}
	pts := res.Series[0].Points
	if len(pts) != 2 {
		t.Fatalf("got %d buckets, want 2: %+v", len(pts), pts)
	}
	// [1000,1020) merges the two agent buckets into {1,2,3,100,200,300}, so
	// p99 lands on rank 0.99*5 = 4.95 -> 200. Without the merge it would be
	// 3, which is the point of the assertion.
	if math.Abs(pts[0].V-200) > 0.01*200 {
		t.Errorf("first bucket = %v, want ~200 — the two sub-buckets should merge", pts[0].V)
	}
	if math.Abs(pts[1].V-7) > 0.01*7 {
		t.Errorf("second bucket = %v, want ~7", pts[1].V)
	}
}

func TestPercentile_EmptyBucketIsAGap(t *testing.T) {
	f := newFixture()
	f.add(t, "lat", nil, 1000, 1, 2, 3)
	res := f.run(t, Request{Metric: "lat", Agg: P50, From: 1000, To: 1029, Interval: 10})
	pts := res.Series[0].Points
	if len(pts) != 3 {
		t.Fatalf("got %d buckets, want 3", len(pts))
	}
	if math.IsNaN(pts[0].V) {
		t.Errorf("first bucket is a gap, want a value")
	}
	for i := 1; i < 3; i++ {
		if !math.IsNaN(pts[i].V) {
			t.Errorf("bucket %d = %v, want a gap rather than a zero", i, pts[i].V)
		}
	}
}

// A metric with no sketches answers with no series rather than an error: a
// dashboard asking about a window before the metric existed is normal.
func TestPercentile_NoData(t *testing.T) {
	f := newFixture()
	res := f.run(t, Request{Metric: "lat", Agg: P95, From: 1000, To: 1009, Interval: 10})
	if len(res.Series) != 0 {
		t.Errorf("got %+v, want no series", res.Series)
	}
}

// Sketches built at different relative accuracies cannot be merged, and
// answering from whichever subset happened to agree would be the confident
// wrong answer this whole design exists to avoid.
func TestPercentile_RefusesToMergeIncompatibleSketches(t *testing.T) {
	f := newFixture()
	f.add(t, "lat", []string{"host:a"}, 1000, 1, 2, 3)

	other, err := sketch.NewWithGamma(1.5)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Add(4); err != nil {
		t.Fatal(err)
	}
	ref := tsdb.NewSeriesRef("lat", []string{"host:b"})
	f.sketches.byKey[ref.Key()] = []sketchstore.Point{{TimeMs: 1000 * 1000, Sketch: other}}
	f.store.series = append(f.store.series, tsdb.SeriesSamples{
		Series:  tsdb.NewSeriesRef("lat"+wire.SuffixCount, []string{"host:b"}),
		Samples: []tsdb.Sample{{T: 1000 * 1000, V: 1}},
	})

	_, err = Run(context.Background(), f.store, f.sketches,
		Request{Metric: "lat", Kind: wire.KindDistribution, Agg: P95, From: 1000, To: 1009, Interval: 10})
	if !errors.Is(err, sketch.ErrIncompatible) {
		t.Fatalf("got %v, want ErrIncompatible", err)
	}
}

// Merging must not write through into the store's data: the first sketch to
// reach a bucket is the caller's, not the group's.
func TestPercentile_DoesNotMutateTheStoredSketches(t *testing.T) {
	f := newFixture()
	f.add(t, "lat", []string{"host:a"}, 1000, 1, 2, 3)
	f.add(t, "lat", []string{"host:b"}, 1000, 100, 200)

	stored := f.sketches.byKey[tsdb.NewSeriesRef("lat", []string{"host:a"}).Key()][0].Sketch
	before := stored.Count()

	f.run(t, Request{Metric: "lat", Agg: P95, From: 1000, To: 1009, Interval: 10})

	if got := stored.Count(); got != before {
		t.Errorf("a stored sketch grew from %v to %v during a query", before, got)
	}
}

func TestPercentile_StoreErrors(t *testing.T) {
	t.Run("no sketch store", func(t *testing.T) {
		_, err := Run(context.Background(), &memStore{}, nil,
			Request{Metric: "lat", Kind: wire.KindDistribution, Agg: P95, From: 1000, To: 1009, Interval: 10})
		if !errors.Is(err, ErrNoSketchStore) {
			t.Errorf("got %v, want ErrNoSketchStore", err)
		}
	})
	t.Run("select fails", func(t *testing.T) {
		boom := errors.New("boom")
		_, err := Run(context.Background(), &memStore{err: boom}, &memSketches{},
			Request{Metric: "lat", Kind: wire.KindDistribution, Agg: P95, From: 1000, To: 1009, Interval: 10})
		if !errors.Is(err, boom) {
			t.Errorf("got %v, want boom", err)
		}
	})
	t.Run("sketch read fails", func(t *testing.T) {
		f := newFixture()
		f.add(t, "lat", nil, 1000, 1)
		boom := errors.New("boom")
		f.sketches.err = boom
		_, err := Run(context.Background(), f.store, f.sketches,
			Request{Metric: "lat", Kind: wire.KindDistribution, Agg: P95, From: 1000, To: 1009, Interval: 10})
		if !errors.Is(err, boom) {
			t.Errorf("got %v, want boom", err)
		}
	})
}

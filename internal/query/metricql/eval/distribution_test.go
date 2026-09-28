package eval

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/sketch"
)

func dist(t *testing.T, e *Evaluator, q string, from, to, interval int64) (Distribution, error) {
	t.Helper()
	n, err := metricql.Parse(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return e.Distribution(context.Background(), Request{Expr: n, From: from, To: to, Interval: interval})
}

// The shape of the answer, and the two things a heatmap needs from it: bins
// that span the observed values, and exact aggregates beside them.
func TestDistribution_BinsSpanTheValues(t *testing.T) {
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{
		"host:a": {0: sketchOf(t, 1, 2, 4, 8), 10: sketchOf(t, 16)},
	})
	res, err := dist(t, e, "dist:lat{*}", 0, 20, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) != 1 {
		t.Fatalf("%d series, want 1", len(res.Series))
	}
	s := res.Series[0]
	if s.Scope != "*" || s.Metric != "lat" {
		t.Errorf("%+v", s)
	}
	if len(s.Buckets) != 2 {
		t.Fatalf("%d buckets, want 2", len(s.Buckets))
	}

	first := s.Buckets[0]
	// The aggregates are exact — they are carried, not estimated from bins.
	if first.Count != 4 || first.Sum != 15 || first.Min != 1 || first.Max != 8 {
		t.Errorf("aggregates are not exact: %+v", first)
	}
	if len(first.Bins) != 4 {
		t.Errorf("%d bins for four distinct values: %+v", len(first.Bins), first.Bins)
	}
	// Every value must fall inside the bin that claims it, and the bins must
	// ascend. A mirrored or mis-ordered axis is the failure this catches.
	total := 0.0
	for i, b := range first.Bins {
		if b.Lower > b.Upper {
			t.Errorf("bin %d is inverted: %v", i, b)
		}
		if i > 0 && b.Lower < first.Bins[i-1].Lower {
			t.Errorf("bin %d is out of order: %v after %v", i, b, first.Bins[i-1])
		}
		total += b.Count
	}
	if total != first.Count {
		t.Errorf("bins hold %g observations, the bucket counts %g", total, first.Count)
	}
	for _, v := range []float64{1, 2, 4, 8} {
		found := false
		for _, b := range first.Bins {
			if v > b.Lower && v <= b.Upper {
				found = true
			}
		}
		if !found {
			t.Errorf("%g falls in no bin: %v", v, first.Bins)
		}
	}
	// Gamma describes the stored sketches, and is the error bar on every bin.
	if res.Gamma <= 1 {
		t.Errorf("gamma = %v", res.Gamma)
	}
	if res.Bins != 5 {
		t.Errorf("total bins = %d, want 5", res.Bins)
	}
}

// Negative values and zero. The sketch stores a negative bucket's index by its
// absolute value, so ascending index is *descending* value; rendering them
// without reversing draws a mirrored histogram, which looks plausible.
func TestDistribution_NegativesAndZeroAreInValueOrder(t *testing.T) {
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{
		"host:a": {0: sketchOf(t, -8, -2, 0, 3, 9)},
	})
	res, err := dist(t, e, "dist:lat{*}", 0, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	bins := res.Series[0].Buckets[0].Bins
	for i := 1; i < len(bins); i++ {
		if bins[i].Lower < bins[i-1].Lower {
			t.Fatalf("bins are not in value order: %v", bins)
		}
	}
	// Zero has a bin of its own: log of zero is undefined, so it cannot live
	// in a bucket with anything else.
	zeros := 0
	for _, b := range bins {
		if b.Lower == 0 && b.Upper == 0 {
			zeros++
			if b.Count != 1 {
				t.Errorf("the zero bin counts %g", b.Count)
			}
		}
	}
	if zeros != 1 {
		t.Errorf("%d zero bins, want exactly 1: %v", zeros, bins)
	}
	// And each value is inside the bin claiming it, on both sides of zero.
	for _, v := range []float64{-8, -2, 3, 9} {
		found := false
		for _, b := range bins {
			if v > b.Lower && v <= b.Upper {
				found = true
			}
		}
		if !found {
			t.Errorf("%g falls in no bin: %v", v, bins)
		}
	}
}

// Merging across series is the whole point, as it is for percentiles: the
// distribution of two hosts is the merge, and its bins must hold every
// observation from both.
func TestDistribution_MergesAcrossSeries(t *testing.T) {
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{
		"host:a": {0: sketchOf(t, 1, 1, 1)},
		"host:b": {0: sketchOf(t, 100, 100)},
	})
	res, err := dist(t, e, "dist:lat{*}", 0, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) != 1 {
		t.Fatalf("%d series, want one merged group", len(res.Series))
	}
	b := res.Series[0].Buckets[0]
	if b.Count != 5 || b.Min != 1 || b.Max != 100 {
		t.Errorf("the merge lost observations: %+v", b)
	}

	// Grouped, the two are separate distributions again.
	res, err = dist(t, e, "dist:lat{*} by {host}", 0, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) != 2 {
		t.Fatalf("%d series, want one per host", len(res.Series))
	}
	for _, s := range res.Series {
		if s.Buckets[0].Count == 5 {
			t.Errorf("%s was merged with its neighbour", s.Scope)
		}
	}
}

// A bucket nothing landed in is absent rather than empty: a heatmap wants a
// gap, every bucket carries its own timestamp, and 1500 empty objects saying
// "nothing happened" would be most of the response on a quiet metric.
func TestDistribution_EmptyBucketsAreAbsent(t *testing.T) {
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{
		"host:a": {0: sketchOf(t, 1), 30: sketchOf(t, 2)},
	})
	res, err := dist(t, e, "dist:lat{*}", 0, 40, 10)
	if err != nil {
		t.Fatal(err)
	}
	buckets := res.Series[0].Buckets
	if len(buckets) != 2 {
		t.Fatalf("%d buckets over a window with two, want 2", len(buckets))
	}
	if buckets[0].T >= buckets[1].T {
		t.Errorf("buckets are not in time order: %v", buckets)
	}
	// The timestamps are what make the gap unambiguous.
	if buckets[1].T-buckets[0].T != 30_000 {
		t.Errorf("the gap is %dms, want 30000", buckets[1].T-buckets[0].T)
	}
}

// `dist:` answers a distribution, so it is not a number and cannot be one.
// Both directions are refused with a message that says where the question goes.
func TestDistribution_RefusesWhatItCannotAnswer(t *testing.T) {
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{
		"host:a": {0: sketchOf(t, 1)},
	})
	for _, tc := range []struct{ name, query, want string }{
		{"a number, not a distribution", "p95:lat{*}", "/api/v1/query"},
		{"an expression of two", "dist:lat{*} / dist:lat{*}", "cannot be added or divided"},
		{"a sum of one", "dist:lat{*} + 1", "cannot be added or divided"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := dist(t, e, tc.query, 0, 10, 10)
			if err == nil {
				t.Fatalf("%s was accepted", tc.query)
			}
			if !errors.Is(err, ErrBadQuery) {
				t.Errorf("%v is not the caller's fault", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%v does not mention %q", err, tc.want)
			}
		})
	}

	// And the other way: a distribution has no place on the numeric path.
	_, err := runErr(e, "dist:lat{*}", 0, 10, 10, nil)
	if err == nil {
		t.Fatal("dist: was evaluated as a number")
	}
	if !strings.Contains(err.Error(), "/api/v1/query/sketch") {
		t.Errorf("%v does not say where it belongs", err)
	}
}

// The response is bounded. A sketch's bin count grows with the ratio between
// its largest and smallest value, so a wide metric over many buckets is an
// enormous answer nobody asked for — and the error names both dials, because
// neither is visible from outside.
func TestDistribution_RefusesAnUnboundedAnswer(t *testing.T) {
	// A sketch only holds bins it has observations for, so a wide *range* is
	// not enough — the values have to be spaced finely enough to land in
	// different buckets. Stepping by just over γ does that: twelve orders of
	// magnitude at γ ≈ 1.0202 is about 1300 bins, and two hundred output
	// buckets of them is past the limit.
	//
	// That is the point of the limit. The size of this answer is set by the
	// *ratio* between the smallest and largest value, not by how many
	// observations there were, and nothing about the query shows it.
	//
	// One sketch, referenced from every bucket: the merge clones before it
	// writes, so nothing here is shared mutably, and building 200 copies of it
	// would only make the test slower.
	wide := map[int64]*sketch.Sketch{}
	one := sketch.NewDefault()
	for v := 1.0; v < 1e12; v *= 1.021 {
		if err := one.Add(v); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(one.PositiveBins()); n < 1000 {
		t.Fatalf("the fixture has only %d bins; it cannot reach the limit", n)
	}
	for ts := int64(0); ts <= 2000; ts += 10 {
		wide[ts] = one
	}
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{"host:a": wide})

	_, err := dist(t, e, "dist:lat{*}", 0, 2000, 10)
	if err == nil {
		t.Fatal("an unbounded distribution was served")
	}
	if !errors.Is(err, ErrBadQuery) {
		t.Errorf("%v is not classified as the caller's", err)
	}
	for _, want := range []string{"coarser interval", "bins"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v does not mention %q", err, want)
		}
	}
}

// A server with no sketch store cannot answer any of these — where on
// /api/v1/query only a percentile needs one.
func TestDistribution_NoSketchStoreIsItsOwnError(t *testing.T) {
	e := sketchEnv(t, map[string]map[int64]*sketch.Sketch{"host:a": {0: sketchOf(t, 1)}})
	e.Sketches = nil
	if _, err := dist(t, e, "dist:lat{*}", 0, 10, 10); !errors.Is(err, ErrNoSketchStore) {
		t.Errorf("%v, want ErrNoSketchStore", err)
	}
}

// The bins are the heatmap's rows, so they have to survive the wire intact.
func TestBin_RoundTrips(t *testing.T) {
	for _, b := range []Bin{
		{Lower: 0, Upper: 0, Count: 3},
		{Lower: -8.5, Upper: -8, Count: 1},
		{Lower: 1e-9, Upper: 1.02e-9, Count: 2},
		{Lower: 1e12, Upper: 1.02e12, Count: 1e9},
	} {
		raw, err := b.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		var got Bin
		if err := got.UnmarshalJSON(raw); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if got != b {
			t.Errorf("%s decoded to %+v, want %+v", raw, got, b)
		}
	}
	// A non-finite bound cannot reach a client as bare NaN, which is not JSON.
	raw, err := Bin{Lower: math.NaN(), Upper: math.Inf(1), Count: 1}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != "[null,null,1]" {
		t.Errorf("%s, want nulls", got)
	}
}

package eval

import (
	"errors"
	"math"
	"testing"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// randomStore draws a set of count series over [0, 600) with tags from a small
// alphabet, so that grouping actually groups.
func randomStore(t *rapid.T) *memStore {
	n := rapid.IntRange(1, 12).Draw(t, "series")
	seen := map[string]bool{}
	var out []tsdb.SeriesSamples
	for range n {
		tags := []string{
			"host:" + rapid.SampledFrom([]string{"a", "b", "c"}).Draw(t, "host"),
			"route:" + rapid.SampledFrom([]string{"x", "y"}).Draw(t, "route"),
			"env:" + rapid.SampledFrom([]string{"dev", "prod"}).Draw(t, "env"),
		}
		ref := tsdb.NewSeriesRef("m", tags)
		if seen[ref.Key()] {
			continue
		}
		seen[ref.Key()] = true
		var samples []tsdb.Sample
		for ts := int64(0); ts < 600; ts += 10 {
			if !rapid.Bool().Draw(t, "present") {
				continue // a gap, which is the interesting case
			}
			samples = append(samples, tsdb.Sample{
				T: ts * 1000,
				V: rapid.Float64Range(0, 1000).Draw(t, "v"),
			})
		}
		if len(samples) == 0 {
			continue
		}
		out = append(out, tsdb.SeriesSamples{Series: ref, Samples: samples})
	}
	return &memStore{series: out}
}

func evaluator(store *memStore) *Evaluator {
	return &Evaluator{Store: store, Types: types{"m": wire.KindCount}, Timeout: -1}
}

// Grouping partitions: summing the groups of `sum:m{*} by {k}` has to give
// `sum:m{*}`, because every series is in exactly one group and sum is
// associative. If a series were counted twice, or dropped for having no value
// for k, this is what would notice.
func TestProperty_GroupingPartitions(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		store := randomStore(t)
		if len(store.series) == 0 {
			return
		}
		e := evaluator(store)
		by := rapid.SampledFrom([]string{"host", "route", "env", "nosuch"}).Draw(t, "by")
		interval := rapid.SampledFrom([]int64{10, 30, 60, 300}).Draw(t, "interval")

		total, err := runErr(e, "sum:m{*}", 0, 599, interval, nil)
		if err != nil {
			t.Fatalf("total: %v", err)
		}
		grouped, err := runErr(e, "sum:m{*} by {"+by+"}", 0, 599, interval, nil)
		if err != nil {
			t.Fatalf("grouped: %v", err)
		}
		if len(total.Series) == 0 {
			if len(grouped.Series) != 0 {
				t.Fatalf("no ungrouped line but %d grouped", len(grouped.Series))
			}
			return
		}
		for i := range total.Series[0].Points {
			want := total.Series[0].Points[i].V
			var sum float64
			any := false
			for _, s := range grouped.Series {
				if v := s.Points[i].V; !math.IsNaN(v) {
					sum += v
					any = true
				}
			}
			switch {
			case math.IsNaN(want) && any:
				t.Fatalf("bucket %d: ungrouped is null but the groups sum to %v", i, sum)
			case math.IsNaN(want):
				continue
			case !any:
				t.Fatalf("bucket %d: ungrouped is %v but every group is null", i, want)
			case math.Abs(sum-want) > 1e-6*math.Max(1, math.Abs(want)):
				t.Fatalf("bucket %d: groups sum to %v, ungrouped is %v", i, sum, want)
			}
		}
	})
}

// Bucket alignment does not depend on where the window starts: buckets are
// floored to multiples of the interval, so moving `from` back by whole
// intervals must shift the answer by whole buckets and change nothing else.
// Without the flooring, a chart would redraw differently every second as
// "now" moved.
func TestProperty_BucketsAreAlignedToTheInterval(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		store := randomStore(t)
		if len(store.series) == 0 {
			return
		}
		e := evaluator(store)
		interval := rapid.SampledFrom([]int64{10, 30, 60}).Draw(t, "interval")
		shift := int64(rapid.IntRange(1, 5).Draw(t, "shift")) * interval

		base, err := runErr(e, "sum:m{*}", 600, 1199, interval, nil)
		if err != nil {
			t.Fatal(err)
		}
		moved, err := runErr(e, "sum:m{*}", 600-shift, 1199, interval, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(base.Series) == 0 || len(moved.Series) == 0 {
			return
		}
		// The later window's buckets are a suffix of the earlier one's, with
		// the same timestamps and the same values.
		offset := int(shift / interval)
		for i, p := range base.Series[0].Points {
			q := moved.Series[0].Points[i+offset]
			if p.T != q.T {
				t.Fatalf("bucket %d: timestamps %d and %d", i, p.T, q.T)
			}
			if !sameValue(p.V, q.V) {
				t.Fatalf("bucket %d at %d: %v then %v", i, p.T, p.V, q.V)
			}
		}
	})
}

// Time aggregation before space aggregation, stated as arithmetic: the sum
// across a group of per-series sums is the sum of every sample in the bucket,
// whichever order they are added in. `count` across series is likewise the
// number of series that reported, never the number of samples.
func TestProperty_CountAcrossSeriesCountsSeries(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		store := randomStore(t)
		if len(store.series) == 0 {
			return
		}
		e := evaluator(store)
		interval := rapid.SampledFrom([]int64{10, 60, 300}).Draw(t, "interval")
		res, err := runErr(e, "count:m{*}", 0, 599, interval, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Series) == 0 {
			return
		}
		for i, p := range res.Series[0].Points {
			if math.IsNaN(p.V) {
				continue
			}
			if p.V > float64(len(store.series)) {
				t.Fatalf("bucket %d counts %v series of %d", i, p.V, len(store.series))
			}
			if p.V != math.Trunc(p.V) {
				t.Fatalf("bucket %d counts %v series, which is not a whole number", i, p.V)
			}
		}
	})
}

// Every query the milestone spec gives as an example has to evaluate. They are
// the acceptance criterion, so they are a test rather than a note.
func TestEval_TheMilestonesExampleQueries(t *testing.T) {
	e := fixture()
	for _, q := range []string{
		"avg:req.count{host:a,route:/x} by {route}",
		"sum:req.count{route:/x}.as_rate() / sum:req.count{*}.as_rate() * 100",
		"p95:lat{host:a} by {route}.rollup(max, 60)",
		`top(sum:req.count{host:a} by {host}, 5, "mean", "desc")`,
	} {
		t.Run(q, func(t *testing.T) {
			// lat has no sketch store in the fixture, so that one is expected
			// to refuse with a reason rather than answer.
			_, err := runErr(e, q, 0, 59, 60, nil)
			if err != nil && !isNoSketchStore(err) {
				t.Errorf("%v", err)
			}
		})
	}
}

func isNoSketchStore(err error) bool { return errors.Is(err, ErrNoSketchStore) }

func sameValue(a, b float64) bool {
	if math.IsNaN(a) && math.IsNaN(b) {
		return true
	}
	return a == b
}

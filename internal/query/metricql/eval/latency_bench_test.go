package eval

import (
	"context"
	"fmt"
	"math"
	"sort"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// rangeStore serves each series' samples for the window by slicing, not
// copying, so that what BenchmarkQueryLatency times is the evaluator reading
// points and not a test double allocating them. [memStore] copies on every
// Select, which at a week of ten-second points per series would dominate.
type rangeStore struct {
	tsdb.MetricStore
	series []tsdb.SeriesSamples
}

func (r *rangeStore) Select(_ context.Context, sel tsdb.Selector, from, to int64) (tsdb.SeriesSet, error) {
	out := make([]tsdb.SeriesSamples, 0, len(r.series))
	for _, s := range r.series {
		if !sel.Matches(s.Series) {
			continue
		}
		lo := sort.Search(len(s.Samples), func(i int) bool { return s.Samples[i].T >= from })
		hi := sort.Search(len(s.Samples), func(i int) bool { return s.Samples[i].T > to })
		if lo < hi {
			out = append(out, tsdb.SeriesSamples{Series: s.Series, Samples: s.Samples[lo:hi]})
		}
	}
	return tsdb.NewSliceSet(out), nil
}

// BenchmarkQueryLatency is the plan's L12: how long the evaluator takes to
// answer the canonical widget query, `sum … by {route}.as_rate()`, as the window
// grows (1 hour, 1 day, 7 days) and as the selection grows (10, 100, 1000
// series, which is the per-node limit).
//
// Every series holds one point per ten seconds across the whole window, which is
// the agent's flush rate, each with its own array, so that nothing is shared in
// cache that a real store would not share. Two things are held fixed so that
// each axis moves one thing: the grid is 180 buckets in every cell (an explicit
// interval of window/180, all multiples of the agent's 10 s flush), and the
// query always has 10 groups. What still varies with the series count, by
// construction, is the series per group (1, 10, 100): that is what a larger
// selection means.
//
// What it does not measure: the storage read path. A real [tsdb.MetricStore]
// decodes sealed blocks on the way, and that cost has its own benchmarks in
// the tsdb package. These numbers are the evaluator's share of a query, and the
// floor under a real one. The 7d window holds about 1 GB of samples for its 1000
// series, so it is skipped under -short.
//
// Run: go test -run '^$' -bench QueryLatency -benchmem -benchtime 5x
// ./internal/query/metricql/eval/. Recorded results are in docs/notes/M3.md.
func BenchmarkQueryLatency(b *testing.B) {
	const (
		stepMs  = 10_000
		buckets = 180
		groups  = 10
	)
	windows := []struct {
		name string
		secs int64
	}{{"1h", 3600}, {"1d", 86400}, {"7d", 7 * 86400}}

	for _, w := range windows {
		if w.name == "7d" && testing.Short() {
			continue
		}
		points := int(w.secs * 1000 / stepMs)
		all := make([]tsdb.SeriesSamples, 1000)
		for i := range all {
			smp := make([]tsdb.Sample, points)
			for t := range smp {
				smp[t] = tsdb.Sample{T: int64(t) * stepMs, V: float64(i + t)}
			}
			all[i] = series("req.count",
				[]string{fmt.Sprintf("route:/r%d", i%groups), fmt.Sprintf("host:h%d", i/groups), "service:api"}, smp)
		}

		for _, n := range []int{10, 100, 1000} {
			b.Run(fmt.Sprintf("window=%s/series=%d", w.name, n), func(b *testing.B) {
				e := &Evaluator{
					Store:   &rangeStore{series: all[:n]},
					Types:   types{"req.count": wire.KindCount},
					Timeout: -1,
				}
				node, err := metricql.Parse("sum:req.count{service:api} by {route}.as_rate()")
				if err != nil {
					b.Fatal(err)
				}
				req := Request{Expr: node, From: 0, To: w.secs, Interval: w.secs / buckets}
				res, err := e.Eval(context.Background(), req)
				if err != nil {
					b.Fatal(err)
				}
				// The benchmark times an answer, so check there is one: ten lines, each
				// with a point per bucket and a real, non-zero value somewhere in it.
				if len(res.Series) != groups {
					b.Fatalf("got %d lines, want %d", len(res.Series), groups)
				}
				for _, line := range res.Series {
					nonZero := false
					for _, p := range line.Points {
						if !math.IsNaN(p.V) && p.V != 0 {
							nonZero = true
						}
					}
					if len(line.Points) < buckets || !nonZero {
						b.Fatalf("line %q has %d points, non-zero value: %v: the query is not answering what the benchmark says it times",
							line.Scope, len(line.Points), nonZero)
					}
				}
				b.ReportMetric(float64(n*points), "points/op")
				b.ReportAllocs()
				for b.Loop() {
					if _, err := e.Eval(context.Background(), req); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

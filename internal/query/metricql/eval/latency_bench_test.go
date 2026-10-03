package eval

import (
	"context"
	"fmt"
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
// the agent's flush rate, with its own array, so that nothing is shared in cache
// that a real store would not share. The interval is the planner's default,
// about 300 buckets, so the cells differ in how many points are *read* and not
// in how many are drawn.
//
// What it does not measure: the storage read path. A real [tsdb.MetricStore]
// decodes sealed blocks on the way, and that cost has its own benchmarks in
// the tsdb package. These numbers are the evaluator's share of a query, and the
// floor under a real one. The numbers and the command are in docs/notes/M3.md.
func BenchmarkQueryLatency(b *testing.B) {
	const stepMs = 10_000
	windows := []struct {
		name string
		secs int64
	}{{"1h", 3600}, {"1d", 86400}, {"7d", 7 * 86400}}

	for _, w := range windows {
		for _, n := range []int{10, 100, 1000} {
			b.Run(fmt.Sprintf("window=%s/series=%d", w.name, n), func(b *testing.B) {
				points := int(w.secs * 1000 / stepMs)
				data := make([]tsdb.SeriesSamples, n)
				for i := range data {
					smp := make([]tsdb.Sample, points)
					for t := range smp {
						smp[t] = tsdb.Sample{T: int64(t) * stepMs, V: float64(i + t)}
					}
					data[i] = series("req.count",
						[]string{fmt.Sprintf("route:/r%d", i%20), fmt.Sprintf("host:h%d", i/20), "service:api"}, smp)
				}
				e := &Evaluator{
					Store:   &rangeStore{series: data},
					Types:   types{"req.count": wire.KindCount},
					Timeout: -1,
				}
				node, err := metricql.Parse("sum:req.count{service:api} by {route}.as_rate()")
				if err != nil {
					b.Fatal(err)
				}
				req := Request{Expr: node, From: 0, To: w.secs}
				res, err := e.Eval(context.Background(), req)
				if err != nil {
					b.Fatal(err)
				}
				if len(res.Series) != min(n, 20) {
					b.Fatalf("got %d lines, want %d: the query is not answering what the benchmark says it times", len(res.Series), min(n, 20))
				}
				b.ReportMetric(float64(n*points), "points/op")
				b.ResetTimer()
				for b.Loop() {
					if _, err := e.Eval(context.Background(), req); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

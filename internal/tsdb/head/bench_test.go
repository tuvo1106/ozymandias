package head

import (
	"fmt"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// BenchmarkHead_Append is the intake hot path with the log out of the way, so
// what it measures is the head's own cost: resolving a series through the
// stripes, and encoding a sample.
func BenchmarkHead_Append(b *testing.B) {
	for _, series := range []int{1, 100, 10_000} {
		b.Run(fmt.Sprintf("series=%d", series), func(b *testing.B) {
			h := New(Options{BlockRange: 1 << 40, MaxSeriesPerMetric: -1})
			refs := make([]tsdb.SeriesRef, series)
			for i := range refs {
				refs[i] = tsdb.NewSeriesRef("http.request.count",
					[]string{"env:prod", fmt.Sprintf("host:h%d", i)})
			}
			one := make([]tsdb.SeriesRef, 1)
			sample := make([]Sample, 1)
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; b.Loop(); i++ {
				one[0] = refs[i%series]
				sample[0] = Sample{T: int64(i/series) * 10_000, V: float64(i)}
				h.Append(one, sample)
			}
		})
	}
}

// A whole intake batch at once, which is how the agent actually writes.
func BenchmarkHead_AppendBatch(b *testing.B) {
	const series = 1000
	h := New(Options{BlockRange: 1 << 40, MaxSeriesPerMetric: -1})
	refs := make([]tsdb.SeriesRef, series)
	for i := range refs {
		refs[i] = tsdb.NewSeriesRef("http.request.count",
			[]string{"env:prod", fmt.Sprintf("host:h%d", i)})
	}
	samples := make([]Sample, series)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; b.Loop(); i++ {
		for j := range samples {
			samples[j] = Sample{T: int64(i) * 10_000, V: float64(j)}
		}
		h.Append(refs, samples)
	}
	b.ReportMetric(float64(series), "samples/op")
}

func BenchmarkHead_Select(b *testing.B) {
	h := New(Options{BlockRange: 1 << 40, MaxSeriesPerMetric: -1})
	const series, samples = 1000, 240
	refs := make([]tsdb.SeriesRef, series)
	for i := range refs {
		refs[i] = tsdb.NewSeriesRef("http.request.count",
			[]string{"env:prod", fmt.Sprintf("host:h%d", i)})
	}
	one, s := make([]tsdb.SeriesRef, 1), make([]Sample, 1)
	for t := 0; t < samples; t++ {
		for i := range refs {
			one[0], s[0] = refs[i], Sample{T: int64(t) * 10_000, V: float64(t)}
			h.Append(one, s)
		}
	}

	for _, c := range []struct {
		name string
		sel  tsdb.Selector
	}{
		{"all", tsdb.Selector{Metric: "http.request.count"}},
		{"one series", tsdb.Selector{Metric: "http.request.count", Matchers: []tsdb.Matcher{
			{Key: "host", Value: "h500", Type: tsdb.Equal}}}},
		{"wildcard", tsdb.Selector{Metric: "http.request.count", Matchers: []tsdb.Matcher{
			{Key: "host", Value: "h1*", Type: tsdb.Wildcard}}}},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			var got int
			for b.Loop() {
				got = 0
				for _, ss := range mustSelect(b, h, c.sel, 0, 1<<40) {
					got += len(ss.Samples)
				}
			}
			b.ReportMetric(float64(got), "samples/op")
		})
	}
}

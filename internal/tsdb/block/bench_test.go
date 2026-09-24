package block

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

func benchSeries(n, samples int) []tsdb.SeriesSamples {
	out := make([]tsdb.SeriesSamples, 0, n)
	for i := 0; i < n; i++ {
		s := tsdb.SeriesSamples{
			Series: tsdb.NewSeriesRef("http.request.duration",
				[]string{"env:production-us-east-1", fmt.Sprintf("host:h%04d", i)}),
			Samples: make([]tsdb.Sample, samples),
		}
		for j := range s.Samples {
			s.Samples[j] = tsdb.Sample{T: int64(j) * 10_000, V: 0.5 + float64(j%97)*1e-9}
		}
		out = append(out, s)
	}
	return out
}

// Writing a block is what a head cut costs. It happens once per block range,
// so the number that matters is bytes per sample as much as time.
func BenchmarkWrite(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprintf("series=%d", n), func(b *testing.B) {
			series := benchSeries(n, 240)
			b.ReportAllocs()
			b.ResetTimer()
			var meta Meta
			var dir string
			for b.Loop() {
				parent := b.TempDir()
				m, err := Write(parent, series, WriterOptions{})
				if err != nil {
					b.Fatal(err)
				}
				meta, dir = m, filepath.Join(parent, m.ULID.String())
			}
			b.StopTimer()
			chunks := fileSizeB(b, filepath.Join(dir, ChunksFilename))
			index := fileSizeB(b, filepath.Join(dir, IndexFilename))
			total := float64(chunks + index)
			b.ReportMetric(total/float64(meta.Stats.Samples), "B/sample")
			b.ReportMetric(float64(index)/total*100, "%-index")
		})
	}
}

// Opening a block is paid once per block per process start, and it is when the
// whole index is parsed — the cost of not memory-mapping it.
func BenchmarkOpen(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(fmt.Sprintf("series=%d", n), func(b *testing.B) {
			parent := b.TempDir()
			m, err := Write(parent, benchSeries(n, 240), WriterOptions{})
			if err != nil {
				b.Fatal(err)
			}
			dir := filepath.Join(parent, m.ULID.String())
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				blk, err := Open(dir)
				if err != nil {
					b.Fatal(err)
				}
				_ = blk.Close()
			}
		})
	}
}

// Querying: the case the per-chunk time ranges exist for is a short window
// against a wide block.
func BenchmarkSelect(b *testing.B) {
	parent := b.TempDir()
	m, err := Write(parent, benchSeries(1000, 240), WriterOptions{})
	if err != nil {
		b.Fatal(err)
	}
	blk, err := Open(filepath.Join(parent, m.ULID.String()))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = blk.Close() }()

	for _, c := range []struct {
		name     string
		sel      tsdb.Selector
		from, to int64
	}{
		{"one series, whole block", tsdb.Selector{Metric: "http.request.duration",
			Matchers: []tsdb.Matcher{{Key: "host", Value: "h0500", Type: tsdb.Equal}}},
			math.MinInt64, math.MaxInt64},
		{"one series, short window", tsdb.Selector{Metric: "http.request.duration",
			Matchers: []tsdb.Matcher{{Key: "host", Value: "h0500", Type: tsdb.Equal}}},
			2_000_000, 2_100_000},
		{"all series, whole block", tsdb.Selector{Metric: "http.request.duration"},
			math.MinInt64, math.MaxInt64},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			var got int
			for b.Loop() {
				res, err := blk.Select(c.sel, c.from, c.to)
				if err != nil {
					b.Fatal(err)
				}
				got = 0
				for _, s := range res {
					got += len(s.Samples)
				}
			}
			b.ReportMetric(float64(got), "samples/op")
		})
	}
}

func fileSizeB(b *testing.B, path string) int64 {
	b.Helper()
	info, err := os.Stat(path)
	if err != nil {
		b.Fatal(err)
	}
	return info.Size()
}

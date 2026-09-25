package sketchstore

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// benchSketch is a realistic latency sketch: a few hundred observations in a
// narrow band, which is what one series produces in one 10s bucket.
func benchSketch(n int) *sketch.Sketch {
	rng := rand.New(rand.NewSource(1))
	s := sketch.NewDefault()
	for i := 0; i < n; i++ {
		_ = s.Add(rng.ExpFloat64()*20 + 1)
	}
	return s
}

// Encoding runs once per sketch per intake request, so it is on the write
// path of every distribution in the fleet.
func BenchmarkEncodeValue(b *testing.B) {
	for _, n := range []int{10, 300, 10_000} {
		s := benchSketch(n)
		b.Run(fmt.Sprintf("observations=%d", n), func(b *testing.B) {
			var v []byte
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var err error
				if v, err = encodeValue(s); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			// What one sketch costs on disk, which is the number that
			// decides whether distributions are affordable at cardinality.
			b.ReportMetric(float64(len(v)), "stored_bytes")
			b.ReportMetric(float64(len(s.PositiveBins())), "buckets")
		})
	}
}

// Decoding runs once per sketch per query bucket, so a p95 over an hour and
// fifty hosts does it a few hundred times.
func BenchmarkDecodeValue(b *testing.B) {
	v, err := encodeValue(benchSketch(300))
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := decodeValue(v); err != nil {
			b.Fatal(err)
		}
	}
}

// One intake request's worth of sketches: a hundred series, one bucket each.
func BenchmarkAppend(b *testing.B) {
	s, err := Open(Options{Dir: filepath.Join(b.TempDir(), "sketches")})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // benchmark
	entries := make([]Entry, 100)
	for i := range entries {
		entries[i] = Entry{
			Series: tsdb.NewSeriesRef("lat", []string{fmt.Sprintf("host:h%d", i)}),
			Points: []Point{{TimeMs: 0, Sketch: benchSketch(300)}},
		}
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := range entries {
			entries[j].Points[0].TimeMs = int64(i+1) * 10_000
		}
		if _, err := s.Append(ctx, entries); err != nil {
			b.Fatal(err)
		}
	}
}

// One series' hour: 360 buckets read and decoded, which is what a percentile
// query pays per series.
func BenchmarkReadAnHour(b *testing.B) {
	s, err := Open(Options{Dir: filepath.Join(b.TempDir(), "sketches")})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close() //nolint:errcheck // benchmark
	ref := tsdb.NewSeriesRef("lat", []string{"host:h1"})
	points := make([]Point, 360)
	for i := range points {
		points[i] = Point{TimeMs: int64(i) * 10_000, Sketch: benchSketch(300)}
	}
	if _, err := s.Append(context.Background(), []Entry{{Series: ref, Points: points}}); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := s.Read(ctx, ref, 0, 3600*1000)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != len(points) {
			b.Fatalf("read %d points, want %d", len(got), len(points))
		}
	}
}

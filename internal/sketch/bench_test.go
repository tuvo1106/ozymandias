package sketch

import (
	"math/rand"
	"testing"
)

// Add is on the agent's hot path: every distribution observation from every
// client passes through it, and the agent is supposed to be the cheap part of
// the system. What it costs is one log, one ceil and a slice index.
func BenchmarkAdd(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	values := make([]float64, 1024)
	for i := range values {
		values[i] = rng.ExpFloat64() * 100
	}
	s := NewDefault()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Add(values[i%len(values)])
	}
}

// BenchmarkAddWideRange is the same work against values spread over eight
// orders of magnitude, which is what makes the store grow and occasionally
// reallocate rather than settling into a steady band.
func BenchmarkAddWideRange(b *testing.B) {
	rng := rand.New(rand.NewSource(2))
	values := make([]float64, 1024)
	for i := range values {
		values[i] = rng.Float64() * 1e8
	}
	s := NewDefault()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Add(values[i%len(values)])
	}
}

// Merge is the query path's cost: a p95 over an hour and fifty hosts merges
// hundreds of sketches before it answers.
func BenchmarkMerge(b *testing.B) {
	rng := rand.New(rand.NewSource(3))
	src := NewDefault()
	for i := 0; i < 10_000; i++ {
		_ = src.Add(rng.ExpFloat64() * 100)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst := NewDefault()
		_ = dst.Merge(src)
	}
}

// Quantile walks the buckets once, so it costs the width of the store rather
// than the number of observations — a sketch of a billion points answers as
// fast as a sketch of a thousand.
func BenchmarkQuantile(b *testing.B) {
	rng := rand.New(rand.NewSource(4))
	s := NewDefault()
	for i := 0; i < 100_000; i++ {
		_ = s.Add(rng.ExpFloat64() * 100)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Quantile(0.95)
	}
}

package chunkenc

import "testing"

// The two hot paths: appending a sample to the head's live chunk, and decoding
// a chunk during a query. Everything above them in the store is bookkeeping by
// comparison.

func BenchmarkAppender_Append(b *testing.B) {
	cases := []struct {
		name  string
		value func(i int) float64
	}{
		// A counter that does not move: one bit for the timestamp, one for the
		// value. The cheapest and by far the most common case.
		{"constant", func(int) float64 { return 1 }},
		// A gauge wobbling in its low bits: the window-reuse path.
		{"wobbling", func(i int) float64 { return 0.5 + float64(i%97)*1e-9 }},
		// Values sharing nothing: the full leading/trailing header every time.
		{"random", func(i int) float64 { return float64(i) * 1.7320508 }},
	}
	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			var chunks int
			ch := NewChunk()
			a, _ := ch.Appender()
			n := 0
			for i := 0; b.Loop(); i++ {
				if n == MaxSamplesPerChunk {
					ch = NewChunk()
					a, _ = ch.Appender()
					n, chunks = 0, chunks+1
				}
				_ = a.Append(int64(i)*10_000, c.value(i))
				n++
			}
			b.ReportMetric(float64(chunks), "chunks")
		})
	}
}

func BenchmarkIterator_Next(b *testing.B) {
	ch := NewChunk()
	a, _ := ch.Appender()
	for i := 0; i < MaxSamplesPerChunk; i++ {
		_ = a.Append(int64(i)*10_000, 0.5+float64(i%97)*1e-9)
	}
	encoded := ch.Bytes()
	b.SetBytes(int64(len(encoded)))
	b.ReportAllocs()
	b.ResetTimer()

	var samples int
	for b.Loop() {
		c, _ := FromBytes(encoded)
		it := c.Iterator()
		for it.Next() {
			_, _ = it.At()
			samples++
		}
	}
	b.ReportMetric(float64(len(encoded))/float64(MaxSamplesPerChunk), "B/sample")
}

// BenchmarkCompressionRatio is not a speed test: it records the bytes per
// sample each shape costs, which is the number the format exists to minimize.
func BenchmarkCompressionRatio(b *testing.B) {
	for _, c := range []struct {
		name  string
		value func(i int) float64
	}{
		{"constant", func(int) float64 { return 1 }},
		{"wobbling", func(i int) float64 { return 0.5 + float64(i%97)*1e-9 }},
		{"random", func(i int) float64 { return float64(i) * 1.7320508 }},
	} {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				ch := NewChunk()
				a, _ := ch.Appender()
				for i := 0; i < MaxSamplesPerChunk; i++ {
					_ = a.Append(int64(i)*10_000, c.value(i))
				}
				b.ReportMetric(float64(len(ch.Bytes()))/MaxSamplesPerChunk, "B/sample")
				b.ReportMetric(16/(float64(len(ch.Bytes()))/MaxSamplesPerChunk), "x-vs-raw")
			}
		})
	}
}

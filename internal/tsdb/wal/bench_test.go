package wal

import (
	"fmt"
	"testing"
)

// The write path's cost is dominated by one decision: whether each batch is
// fsynced. These two benchmarks are that decision, measured.
func BenchmarkWAL_Log(b *testing.B) {
	for _, sync := range []bool{false, true} {
		for _, size := range []int{64, 4096} {
			b.Run(fmt.Sprintf("sync=%v/record=%dB", sync, size), func(b *testing.B) {
				w, err := Open(Options{Dir: b.TempDir()})
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = w.Close() }()
				rec := Record{Type: 1, Data: make([]byte, size)}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if err := w.Log(rec); err != nil {
						b.Fatal(err)
					}
					if sync {
						if err := w.Sync(); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
		}
	}
}

// A realistic batch: one Log call carrying a whole intake request's worth of
// samples, which is how group commit actually amortizes the fsync.
func BenchmarkWAL_LogBatchThenSync(b *testing.B) {
	for _, n := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			w, err := Open(Options{Dir: b.TempDir()})
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = w.Close() }()
			recs := make([]Record, n)
			for i := range recs {
				recs[i] = Record{Type: 1, Data: make([]byte, 64)}
			}
			b.SetBytes(int64(n * 64))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := w.Log(recs...); err != nil {
					b.Fatal(err)
				}
				if err := w.Sync(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkReader_Next(b *testing.B) {
	dir := b.TempDir()
	w, err := Open(Options{Dir: dir})
	if err != nil {
		b.Fatal(err)
	}
	recs := make([]Record, 1000)
	for i := range recs {
		recs[i] = Record{Type: 1, Data: make([]byte, 64)}
	}
	if err := w.Log(recs...); err != nil {
		b.Fatal(err)
	}
	if err := w.Close(); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(recs) * 64))
	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		r, err := NewReader(dir)
		if err != nil {
			b.Fatal(err)
		}
		n := 0
		for r.Next() {
			n++
		}
		if err := r.Err(); err != nil {
			b.Fatal(err)
		}
		_ = r.Close()
	}
}

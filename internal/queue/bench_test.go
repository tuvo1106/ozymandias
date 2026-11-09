package queue

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// BenchmarkProduce is the group-commit sweep (M7 §1): throughput and
// per-record latency for several sync windows and producer counts. The
// window trades latency for fewer fsyncs; with one producer it is pure cost,
// with many it is what lets one fsync acknowledge a batch.
//
//	go test ./internal/queue -run '^$' -bench Produce -benchtime 2s
func BenchmarkProduce(b *testing.B) {
	payload := make([]byte, 512)
	for _, window := range []time.Duration{-1, time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond} {
		for _, producers := range []int{1, 16, 128} {
			name := fmt.Sprintf("window=%v/producers=%d", window, producers)
			if window < 0 {
				name = fmt.Sprintf("window=none/producers=%d", producers)
			}
			b.Run(name, func(b *testing.B) {
				tp, err := Open(Options{Dir: b.TempDir(), SyncInterval: window})
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = tp.Close() }()
				b.SetBytes(int64(len(payload)))
				b.ResetTimer()
				sem := make(chan struct{}, producers)
				errs := make(chan error, 1)
				for range b.N {
					sem <- struct{}{}
					go func() {
						defer func() { <-sem }()
						if _, err := tp.Produce(context.Background(), payload); err != nil {
							select {
							case errs <- err:
							default:
							}
						}
					}()
				}
				for range producers {
					sem <- struct{}{}
				}
				select {
				case err := <-errs:
					b.Fatal(err)
				default:
				}
			})
		}
	}
}

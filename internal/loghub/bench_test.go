package loghub

import (
	"testing"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Publish is on the intake's path for every accepted batch, so its cost with
// many tails open is what the 64-subscriber cap protects.
func BenchmarkHub_PublishToSubscribers(b *testing.B) {
	h := New(0)
	for i := 0; i < 32; i++ {
		s, ok := h.Subscribe(nil, 1000)
		if !ok {
			b.Fatal("refused")
		}
		defer s.Close()
	}
	batch := make([]wire.Log, 100)
	for i := range batch {
		batch[i] = mk("web-api", "info", "request handled")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h.Publish(batch)
	}
}

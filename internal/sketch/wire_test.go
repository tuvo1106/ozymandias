package sketch

import (
	"math"
	"testing"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// The wire's bucket cap and the store's have to be the same number. They are
// declared separately — pkg/wire is the public protocol surface and must not
// import an internal package — so nothing but this test stops them drifting,
// and drift means either a payload the store silently collapses or a limit
// that rejects what an agent legitimately sends.
func TestWire_BucketCapsAgree(t *testing.T) {
	if wire.MaxBinsPerSketch != MaxBins {
		t.Fatalf("wire.MaxBinsPerSketch = %d, sketch.MaxBins = %d", wire.MaxBinsPerSketch, MaxBins)
	}
}

// A sketch that goes out on the wire and comes back must answer every
// question identically. Anything less and a percentile depends on whether the
// query hit the agent that built the sketch or the server that received it.
func TestProperty_WireRoundTripPreservesEverything(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		values := rapid.SliceOfN(genValue(), 1, 200).Draw(t, "values")
		orig := sketchOf(t, values)

		got, err := FromWire(orig.ToWire())
		if err != nil {
			t.Fatalf("FromWire: %v", err)
		}

		if got.Gamma() != orig.Gamma() || got.Count() != orig.Count() ||
			got.Sum() != orig.Sum() || got.Min() != orig.Min() ||
			got.Max() != orig.Max() || got.ZeroCount() != orig.ZeroCount() {
			t.Fatalf("aggregates differ:\n got %+v\nwant %+v", got.ToWire(), orig.ToWire())
		}
		for _, q := range []float64{0, 0.1, 0.5, 0.9, 0.99, 1} {
			a, err1 := orig.Quantile(q)
			b, err2 := got.Quantile(q)
			if err1 != nil || err2 != nil {
				t.Fatalf("Quantile(%v): %v %v", q, err1, err2)
			}
			if a != b {
				t.Errorf("q=%v: got %v, want %v", q, b, a)
			}
		}
	})
}

// An empty sketch is the one place the aggregates are not real numbers: min
// and max are the ±Inf sentinels, which JSON cannot write. They travel as
// zeros and have to come back as sentinels, or the first value added after
// the round trip is compared against 0 and the min is wrong forever.
func TestWire_AnEmptySketchKeepsItsSentinels(t *testing.T) {
	w := NewDefault().ToWire()
	if w.Min != 0 || w.Max != 0 {
		t.Fatalf("empty sketch went out as min=%v max=%v, which has no JSON form", w.Min, w.Max)
	}
	if err := wire.ValidateSketch(&w); err != nil {
		t.Fatalf("an empty sketch is not a valid payload: %v", err)
	}

	got, err := FromWire(w)
	if err != nil {
		t.Fatalf("FromWire: %v", err)
	}
	if !math.IsInf(got.Min(), 1) || !math.IsInf(got.Max(), -1) {
		t.Fatalf("sentinels not restored: min=%v max=%v", got.Min(), got.Max())
	}
	mustAdd(t, got, 5)
	if got.Min() != 5 || got.Max() != 5 {
		t.Errorf("after adding 5 to a round-tripped empty sketch: min=%v max=%v, want 5 and 5", got.Min(), got.Max())
	}
}

// Bins are never nil on the way out: a decoder reading `null` and one reading
// `[]` must not be able to disagree about whether there were buckets.
func TestWire_BinsAreAlwaysAnArray(t *testing.T) {
	w := NewDefault().ToWire()
	if w.Bins == nil || w.NegBins == nil {
		t.Fatalf("bins=%v neg_bins=%v; both must be empty arrays, not null", w.Bins, w.NegBins)
	}
}

// FromWire is a decoder entry point: it is handed a struct built from another
// process's bytes, and must refuse anything the store cannot hold rather than
// panicking on it.
func TestWire_FromWireRejectsCorruptPayloads(t *testing.T) {
	for _, tc := range []struct {
		name string
		fix  func(*wire.Sketch)
	}{
		{"gamma of one", func(w *wire.Sketch) { w.Gamma = 1 }},
		{"NaN gamma", func(w *wire.Sketch) { w.Gamma = math.NaN() }},
		{"NaN count", func(w *wire.Sketch) { w.Count = math.NaN() }},
		{"index past what Add can produce", func(w *wire.Sketch) {
			w.Bins = []wire.SketchBin{{Index: math.MaxInt32 + 1, Count: 1}}
		}},
		{"buckets that disagree with the count", func(w *wire.Sketch) {
			w.Bins = []wire.SketchBin{{Index: 1, Count: 99}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewDefault()
			mustAdd(t, s, 1, 2, 3)
			w := s.ToWire()
			tc.fix(&w)
			if got, err := FromWire(w); err == nil {
				t.Fatalf("accepted a corrupt payload: %+v", got.ToWire())
			}
		})
	}
}

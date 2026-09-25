package sketch

import (
	"math"
	"testing"
)

// Add clamps its own indices to ±2^31, so those exact values are the widest a
// legitimate payload can carry — and AddBin must accept them. Rejecting at the
// edge would drop the buckets holding the most extreme values a sketch ever
// records.
func TestSketch_AddBinAcceptsTheWidestIndexAddCanProduce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		index int
	}{
		{"the highest", math.MaxInt32},
		{"the lowest", math.MinInt32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewDefault()
			if err := s.AddBin(Bin{Index: tc.index, Count: 2}, false); err != nil {
				t.Fatalf("AddBin at index %d: %v", tc.index, err)
			}
			bins := s.PositiveBins()
			if len(bins) != 1 || bins[0].Index != tc.index || bins[0].Count != 2 {
				t.Fatalf("bins: got %+v, want one bucket of 2 at %d", bins, tc.index)
			}
		})
	}
}

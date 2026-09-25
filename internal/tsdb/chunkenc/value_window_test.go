package chunkenc

import (
	"math"
	"math/bits"
	"testing"
)

// TestChunk_ValueWindowLeadingZeroClamp exercises the boundary of the
// leading-zero clamp in writeValue.
//
// The leading-zero count is stored in five bits, which hold 0..31, so a count
// of 32 or more is clamped to 31 and the extra zeros are paid for as payload.
// Get the boundary wrong — `leading > 32` instead of `>= 32` — and a count of
// exactly 32 is written unclamped: writeBits(32, 5) keeps only the low five
// bits, so 32 is stored as 0. The decoder then rebuilds the value against a
// window 32 bits wider than the one it was encoded with, and returns a number
// that is not the one that went in.
//
// Random values almost never land there: it needs the XOR of two consecutive
// values to fall in [2^31, 2^32), so neither the property test nor the fuzzer
// had hit it, and mutation testing flagged the comparison as untested.
//
// Each case pins the leading-zero count it is testing, so a future change to
// the values cannot quietly stop exercising the boundary.
func TestChunk_ValueWindowLeadingZeroClamp(t *testing.T) {
	const base = 0x3FF0000000000000 // 1.0

	for _, tc := range []struct {
		name    string
		xor     uint64
		leading int
	}{
		{"just below the clamp", 1 << 32, 31},
		{"exactly at the clamp", 1 << 31, 32},
		{"one past the clamp", 1 << 30, 33},
		{"far past the clamp", 1, 63},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := bits.LeadingZeros64(tc.xor); got != tc.leading {
				t.Fatalf("the case is mis-specified: xor %#x has %d leading zeros, not %d",
					tc.xor, got, tc.leading)
			}
			first := math.Float64frombits(base)
			second := math.Float64frombits(base ^ tc.xor)

			c := NewChunk()
			a, err := c.Appender()
			if err != nil {
				t.Fatal(err)
			}
			if err := a.Append(0, first); err != nil {
				t.Fatal(err)
			}
			if err := a.Append(1000, second); err != nil {
				t.Fatal(err)
			}

			var got []float64
			it := c.Iterator()
			for it.Next() {
				_, v := it.At()
				got = append(got, v)
			}
			if err := it.Err(); err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 {
				t.Fatalf("decoded %d samples, want 2", len(got))
			}
			// Compared as bits: these are exact values, and the failure this
			// guards against corrupts the exponent, which == would also catch
			// but %v would print confusingly.
			if math.Float64bits(got[0]) != base || math.Float64bits(got[1]) != base^tc.xor {
				t.Errorf("round-tripped %#x, %#x; want %#x, %#x",
					math.Float64bits(got[0]), math.Float64bits(got[1]), base, base^tc.xor)
			}
		})
	}
}

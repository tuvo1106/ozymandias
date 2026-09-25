package chunkenc

import "testing"

// TestChunk_DeltaOfDeltaBucketBoundaries pins which bucket every boundary
// value lands in, by cost in bits.
//
// Round-trip tests cannot see this. A delta-of-delta encoded one bucket too
// wide decodes back to exactly the right number — it just spends more bits
// doing it — so every correctness test in this package stays green while the
// format quietly changes and compression gets worse. Mutation testing found
// five surviving mutants on the bucket comparison in writeDOD for that reason:
// turning `dod < -(1<<(b.bits-1))` into `<=` misfiles precisely the value at
// the low edge of each bucket, and nothing noticed.
//
// So the assertion is the cost, which is what the bucket *is*. The numbers
// come from dodBuckets and match docs/formats: one bit for "no change", then
// prefix plus payload for the rest.
func TestChunk_DeltaOfDeltaBucketBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		dod  int64
		bits int
	}{
		{"no change", 0, 1},

		{"14-bit, low edge", -8192, 2 + 14},
		{"14-bit, high edge", 8191, 2 + 14},
		{"14-bit, just inside", -8191, 2 + 14},

		{"17-bit, one below the 14-bit floor", -8193, 3 + 17},
		{"17-bit, one above the 14-bit ceiling", 8192, 3 + 17},
		{"17-bit, low edge", -65536, 3 + 17},
		{"17-bit, high edge", 65535, 3 + 17},

		{"20-bit, one below the 17-bit floor", -65537, 4 + 20},
		{"20-bit, one above the 17-bit ceiling", 65536, 4 + 20},
		{"20-bit, low edge", -524288, 4 + 20},
		{"20-bit, high edge", 524287, 4 + 20},

		{"64-bit, one below the 20-bit floor", -524289, 4 + 64},
		{"64-bit, one above the 20-bit ceiling", 524288, 4 + 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dodCostBits(t, tc.dod); got != tc.bits {
				t.Errorf("a delta-of-delta of %d costs %d bits, want %d — it landed in the "+
					"wrong bucket, which still decodes correctly and still wastes the space",
					tc.dod, got, tc.bits)
			}
		})
	}
}

// dodCostBits reports how many bits the third sample's delta-of-delta costs,
// by differencing against a chunk whose third sample has a delta-of-delta of
// zero. Everything else about the two chunks — header, first timestamp, the
// varint delta, all three values — is identical, so the difference is the
// delta-of-delta encoding and nothing else.
func dodCostBits(t *testing.T, dod int64) int {
	t.Helper()
	const zeroCost = 1 // the "no change" bucket, one bit
	return bitLen(t, threeSamples(t, dod)) - bitLen(t, threeSamples(t, 0)) + zeroCost
}

// threeSamples builds the smallest chunk that reaches the delta-of-delta path:
// two samples to establish a delta, and a third offset from it by dod. The
// value never changes, so each sample after the first spends exactly one bit
// on its value and the timestamps are all that differ.
func threeSamples(t *testing.T, dod int64) *Chunk {
	t.Helper()
	// Large enough that the third timestamp stays positive and increasing for
	// every dod in the table, including the most negative.
	const delta = 1 << 20
	c := NewChunk()
	a, err := c.Appender()
	if err != nil {
		t.Fatal(err)
	}
	for _, ts := range []int64{0, delta, 2*delta + dod} {
		if err := a.Append(ts, 1); err != nil {
			t.Fatalf("appending at %d (dod %d): %v", ts, dod, err)
		}
	}
	return c
}

// bitLen is how many bits the chunk's stream actually holds: count is the
// number of *free* bits left in the last byte.
func bitLen(t *testing.T, c *Chunk) int {
	t.Helper()
	return len(c.b.stream)*8 - int(c.b.count)
}

// TestChunk_BucketBoundariesStillRoundTrip is the other half. The cost
// assertions above would be satisfied by an encoder that spent the right
// number of bits on the wrong value, so every boundary also has to decode
// back to what went in.
func TestChunk_BucketBoundariesStillRoundTrip(t *testing.T) {
	const delta = 1 << 20
	for _, dod := range []int64{
		0, -8192, 8191, -8193, 8192, -65536, 65535,
		-65537, 65536, -524288, 524287, -524289, 524288,
	} {
		c := threeSamples(t, dod)
		want := []int64{0, delta, 2*delta + dod}
		var got []int64
		it := c.Iterator()
		for it.Next() {
			ts, _ := it.At()
			got = append(got, ts)
		}
		if err := it.Err(); err != nil {
			t.Fatalf("dod %d: %v", dod, err)
		}
		if len(got) != len(want) {
			t.Fatalf("dod %d: decoded %d samples, want %d", dod, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("dod %d: sample %d decoded at %d, want %d", dod, i, got[i], want[i])
			}
		}
	}
}

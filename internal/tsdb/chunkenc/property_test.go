package chunkenc

import (
	"math"
	"testing"

	"pgregory.net/rapid"
)

// TestChunk_RoundTripProperty is the encoder's whole contract: whatever goes
// in comes back, bit for bit.
//
// "Bit for bit" is the part that needs a property test rather than a handful
// of examples. The value encoding works on the raw float64 bits, so the cases
// that break it are the ones nobody writes by hand — a signalling NaN whose
// payload must survive, negative zero, denormals whose XOR has a long run of
// leading zeros, and the window-reuse path that only triggers on a particular
// relationship between two consecutive values.
func TestChunk_RoundTripProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		times := genTimestamps().Draw(t, "times")
		values := rapid.SliceOfN(genFloat(), len(times), len(times)).Draw(t, "values")

		c := NewChunk()
		a, err := c.Appender()
		if err != nil {
			t.Fatal(err)
		}
		for i := range times {
			if err := a.Append(times[i], values[i]); err != nil {
				t.Fatalf("Append(%d, %v): %v", times[i], values[i], err)
			}
		}
		if got := c.NumSamples(); got != len(times) {
			t.Fatalf("NumSamples = %d, appended %d", got, len(times))
		}

		// Read back through Bytes/FromBytes, the path a block takes, so the
		// encoding is exercised and not just the in-memory state.
		decoded, err := FromBytes(c.Bytes())
		if err != nil {
			t.Fatalf("FromBytes: %v", err)
		}
		it := decoded.Iterator()
		for i := range times {
			if !it.Next() {
				t.Fatalf("stream ended after %d of %d samples: %v", i, len(times), it.Err())
			}
			gotT, gotV := it.At()
			if gotT != times[i] {
				t.Fatalf("sample %d: t = %d, want %d", i, gotT, times[i])
			}
			// Compare the bits, not the values: NaN != NaN, and -0 == +0,
			// but neither may be silently changed by a round trip.
			if math.Float64bits(gotV) != math.Float64bits(values[i]) {
				t.Fatalf("sample %d at t=%d: v bits = %#016x, want %#016x",
					i, gotT, math.Float64bits(gotV), math.Float64bits(values[i]))
			}
		}
		if it.Next() {
			t.Fatalf("stream yielded more than the %d samples appended", len(times))
		}
		if err := it.Err(); err != nil {
			t.Fatalf("iterator: %v", err)
		}
	})
}

// TestChunk_ResumeIsIndistinguishableFromOneRun checks the bit-cursor restore:
// a chunk written in two halves, with a Bytes/FromBytes/Appender round trip in
// between, must be byte-identical to the same samples written in one go.
//
// This is the bug that made everything after a resumed append decode as
// garbage, because the cursor was restored to a byte boundary and the padding
// bits were left in the stream.
func TestChunk_ResumeIsIndistinguishableFromOneRun(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		times := genTimestamps().Draw(t, "times")
		values := rapid.SliceOfN(genFloat(), len(times), len(times)).Draw(t, "values")
		split := rapid.IntRange(1, len(times)).Draw(t, "split")

		oneGo := NewChunk()
		a, err := oneGo.Appender()
		if err != nil {
			t.Fatal(err)
		}
		for i := range times {
			if err := a.Append(times[i], values[i]); err != nil {
				t.Fatal(err)
			}
		}

		first := NewChunk()
		a, err = first.Appender()
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < split; i++ {
			if err := a.Append(times[i], values[i]); err != nil {
				t.Fatal(err)
			}
		}
		resumed, err := FromBytes(first.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		a, err = resumed.Appender()
		if err != nil {
			t.Fatal(err)
		}
		for i := split; i < len(times); i++ {
			if err := a.Append(times[i], values[i]); err != nil {
				t.Fatalf("append after resume at %d: %v", i, err)
			}
		}
		if got, want := resumed.Bytes(), oneGo.Bytes(); string(got) != string(want) {
			t.Fatalf("resuming at %d of %d produced %d bytes, one run produced %d;\n got %x\nwant %x",
				split, len(times), len(got), len(want), got, want)
		}
	})
}

// TestChunk_TruncationIsAnErrorNotGarbageProperty: every prefix of a real
// chunk either decodes to a prefix of the samples or reports an error. It must
// never invent a sample, because a short read is what a damaged file looks
// like and a fabricated value is worse than a missing one.
func TestChunk_TruncationIsAnErrorNotGarbageProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		times := genTimestamps().Draw(t, "times")
		values := rapid.SliceOfN(genFloat(), len(times), len(times)).Draw(t, "values")

		c := NewChunk()
		a, err := c.Appender()
		if err != nil {
			t.Fatal(err)
		}
		for i := range times {
			if err := a.Append(times[i], values[i]); err != nil {
				t.Fatal(err)
			}
		}
		full := c.Bytes()
		n := rapid.IntRange(0, len(full)).Draw(t, "prefix")

		truncated, err := FromBytes(append([]byte(nil), full[:n]...))
		if err != nil {
			return // refused outright: also fine
		}
		it := truncated.Iterator()
		for i := 0; it.Next(); i++ {
			if i >= len(times) {
				t.Fatalf("a %d-byte prefix yielded more than the %d samples encoded", n, len(times))
			}
			gotT, gotV := it.At()
			if gotT != times[i] || math.Float64bits(gotV) != math.Float64bits(values[i]) {
				t.Fatalf("a %d-byte prefix invented sample %d: (%d, %v), want (%d, %v)",
					n, i, gotT, gotV, times[i], values[i])
			}
		}
	})
}

// genTimestamps draws a strictly increasing run, with gaps chosen to land in
// every delta-of-delta bucket including the 64-bit escape.
func genTimestamps() *rapid.Generator[[]int64] {
	return rapid.Custom(func(t *rapid.T) []int64 {
		n := rapid.IntRange(1, MaxSamplesPerChunk).Draw(t, "n")
		out := make([]int64, 0, n)
		cur := rapid.Int64Range(-1<<40, 1<<40).Draw(t, "t0")
		out = append(out, cur)
		for i := 1; i < n; i++ {
			gap := rapid.OneOf(
				rapid.Just[int64](10_000),          // the steady case: dod 0
				rapid.Int64Range(1, 100),           // small
				rapid.Int64Range(1, 1<<13),         // the 14-bit bucket
				rapid.Int64Range(1, 1<<19),         // the 20-bit bucket
				rapid.Int64Range(1, math.MaxInt32), // the 64-bit escape
			).Draw(t, "gap")
			cur += gap
			out = append(out, cur)
		}
		return out
	})
}

// genFloat draws the values that break naive XOR encoders.
func genFloat() *rapid.Generator[float64] {
	return rapid.OneOf(
		rapid.Just(0.0),
		rapid.Just(math.Copysign(0, -1)), // negative zero
		rapid.Just(math.Inf(1)),
		rapid.Just(math.Inf(-1)),
		rapid.Just(math.NaN()),
		rapid.Just(math.Float64frombits(0x7ff8000000000001)), // a NaN with a payload
		rapid.Just(math.SmallestNonzeroFloat64),              // denormal
		rapid.Just(math.MaxFloat64),
		rapid.Just(1.0),
		rapid.Float64(),
		rapid.Custom(func(t *rapid.T) float64 {
			// Values close together, so the window-reuse path is taken.
			return 0.5 + float64(rapid.IntRange(0, 100).Draw(t, "wobble"))*1e-9
		}),
	)
}

// TestChunk_ResumeAChunkWithNoSpareCapacity pins a panic.
//
// Resuming restores the writer's bit cursor from the reader's. When a chunk
// ends exactly on a byte boundary the reader has advanced to a byte that does
// not exist yet, and keeping it unconditionally asked for one byte more than
// the slice had. With a chunk built by appending there is spare capacity and
// the stray byte is merely wrong; with a chunk read out of a block there is
// none, and it panics — in the middle of a query.
func TestChunk_ResumeAChunkWithNoSpareCapacity(t *testing.T) {
	c := NewChunk()
	a, err := c.Appender()
	if err != nil {
		t.Fatal(err)
	}
	// One sample: varint timestamp plus a whole float64 ends exactly on a
	// byte boundary, which is the case where the old cursor arithmetic ran
	// past the end of the slice.
	if err := a.Append(1000, 1.0); err != nil {
		t.Fatal(err)
	}
	// An exact-capacity copy, which is what reading a record out of a block's
	// chunks.dat produces.
	exact := make([]byte, len(c.Bytes()))
	copy(exact, c.Bytes())
	if cap(exact) != len(exact) {
		t.Fatalf("the copy has spare capacity (%d > %d); the test proves nothing", cap(exact), len(exact))
	}
	resumed, err := FromBytes(exact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Appender(); err != nil {
		t.Fatalf("resuming: %v", err)
	}
}

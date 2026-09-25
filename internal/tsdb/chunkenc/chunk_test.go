package chunkenc

import (
	"bytes"
	"math"
	"testing"
)

type sample struct {
	t int64
	v float64
}

// appendAll writes samples into a fresh chunk, failing on the first error.
func appendAll(t *testing.T, samples []sample) *Chunk {
	t.Helper()
	c := NewChunk()
	a, err := c.Appender()
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range samples {
		if err := a.Append(s.t, s.v); err != nil {
			t.Fatalf("append %d (%d, %v): %v", i, s.t, s.v, err)
		}
	}
	return c
}

// readAll decodes a chunk, failing on a decode error.
func readAll(t *testing.T, c *Chunk) []sample {
	t.Helper()
	var got []sample
	it := c.Iterator()
	for it.Next() {
		ts, v := it.At()
		got = append(got, sample{ts, v})
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return got
}

// equalSamples compares by bit pattern, not by ==: NaN != NaN, and -0 == 0,
// yet both must survive a round trip exactly.
func equalSamples(a, b []sample) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].t != b[i].t || math.Float64bits(a[i].v) != math.Float64bits(b[i].v) {
			return false
		}
	}
	return true
}

func TestChunk_RoundTrip(t *testing.T) {
	// Each case is chosen to exercise a specific branch of the encoder; the
	// names say which, because a failure here should point at the branch.
	cases := []struct {
		name    string
		samples []sample
	}{
		{"empty", nil},
		{"single sample", []sample{{1000, 1}}},
		{"two samples set the delta", []sample{{1000, 1}, {11000, 2}}},
		{"perfectly regular: every dod is zero", regular(10, 1000, 10000, 1)},
		{"constant value: every value bit is zero", constant(10, 42)},
		{"dod bucket 14 bits", []sample{{0, 1}, {10000, 1}, {20000, 1}, {28000, 1}}},
		{"dod bucket 17 bits", []sample{{0, 1}, {10000, 1}, {20000, 1}, {90000, 1}}},
		{"dod bucket 20 bits", []sample{{0, 1}, {10000, 1}, {20000, 1}, {600000, 1}}},
		{"dod bucket 64 bits", []sample{{0, 1}, {10000, 1}, {20000, 1}, {5_000_000_000, 1}}},
		{"negative dod", []sample{{0, 1}, {50000, 1}, {60000, 1}, {61000, 1}}},
		{"value window reuse", []sample{{0, 1.5}, {10, 1.75}, {20, 1.625}}},
		{"value window change", []sample{{0, 1}, {10, 1e300}, {20, 1e-300}}},
		{"NaN survives", []sample{{0, math.NaN()}, {10, 1}, {20, math.NaN()}}},
		{"infinities survive", []sample{{0, math.Inf(1)}, {10, math.Inf(-1)}}},
		{"negative zero is not zero", []sample{{0, math.Copysign(0, -1)}, {10, 0}}},
		{"denormals survive", []sample{{0, math.SmallestNonzeroFloat64}, {10, math.MaxFloat64}}},
		{"negative timestamps", []sample{{-5000, 1}, {-4000, 2}, {-3000, 3}}},
		{"full chunk", regular(MaxSamplesPerChunk, 0, 10000, 3.25)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := appendAll(t, tc.samples)
			if got, want := c.NumSamples(), len(tc.samples); got != want {
				t.Errorf("NumSamples = %d, want %d", got, want)
			}
			if got := readAll(t, c); !equalSamples(got, tc.samples) {
				t.Errorf("round trip mismatch\n got %v\nwant %v", got, tc.samples)
			}
			// Same again through the on-disk form, which is how a block reads it.
			raw, err := FromBytes(c.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			if got := readAll(t, raw); !equalSamples(got, tc.samples) {
				t.Errorf("round trip via Bytes mismatch\n got %v\nwant %v", got, tc.samples)
			}
		})
	}
}

func regular(n int, t0, step int64, v float64) []sample {
	out := make([]sample, n)
	for i := range out {
		out[i] = sample{t0 + int64(i)*step, v + float64(i)}
	}
	return out
}

func constant(n int, v float64) []sample {
	out := make([]sample, n)
	for i := range out {
		out[i] = sample{int64(i) * 10000, v}
	}
	return out
}

func TestChunk_RejectsOutOfOrderAndDuplicateTimestamps(t *testing.T) {
	// The whole layout assumes time only moves forward; going backwards is not
	// representable, so it must be refused rather than silently mis-encoded.
	for _, tc := range []struct {
		name   string
		second int64
	}{
		{"equal", 1000},
		{"earlier", 999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewChunk()
			a, _ := c.Appender()
			if err := a.Append(1000, 1); err != nil {
				t.Fatal(err)
			}
			if err := a.Append(tc.second, 2); err == nil {
				t.Fatal("expected an error appending a non-increasing timestamp")
			}
			if c.NumSamples() != 1 {
				t.Errorf("a rejected append changed the chunk: %d samples", c.NumSamples())
			}
		})
	}
	// Same rule from the third sample on, where the delta-of-delta path runs
	// — and both halves of it. Only the "earlier" case used to be here, which
	// left `t <= a.t` on that path tested in one direction: mutation testing
	// turned it into `t < a.t`, so the encoder accepted a *duplicate*
	// timestamp from the third sample on, and the whole suite stayed green.
	// The table above covers both cases for the first two samples; this is
	// the same table, for the path that encodes differently.
	for _, tc := range []struct {
		name  string
		third int64
	}{
		{"equal", 10},
		{"earlier", 5},
	} {
		t.Run("dod path, "+tc.name, func(t *testing.T) {
			c := appendAll(t, []sample{{0, 1}, {10, 2}})
			a, _ := c.Appender()
			if err := a.Append(tc.third, 3); err == nil {
				t.Fatal("expected an error on the delta-of-delta path too")
			}
			if c.NumSamples() != 2 {
				t.Errorf("a rejected append changed the chunk: %d samples", c.NumSamples())
			}
		})
	}
}

func TestChunk_FullRefusesMore(t *testing.T) {
	c := appendAll(t, regular(MaxSamplesPerChunk, 0, 10000, 1))
	if !c.Full() {
		t.Fatal("chunk should report full")
	}
	a, err := c.Appender()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Append(math.MaxInt32, 1); err == nil {
		t.Fatal("expected an error appending to a full chunk")
	}
}

func TestChunk_AppenderResumesAnExistingChunk(t *testing.T) {
	// A head chunk is appended to across restarts of the appender; resuming
	// must reconstruct the encoder state exactly, or the next sample decodes
	// as garbage.
	first := []sample{{0, 1}, {10000, 2}, {20000, 2}, {30000, 7.5}}
	c := appendAll(t, first)

	reopened, err := FromBytes(c.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	a, err := reopened.Appender()
	if err != nil {
		t.Fatal(err)
	}
	more := []sample{{40000, 7.5}, {50000, 8.25}, {75000, 8.25}}
	for _, s := range more {
		if err := a.Append(s.t, s.v); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := readAll(t, reopened), append(append([]sample{}, first...), more...); !equalSamples(got, want) {
		t.Errorf("resumed chunk mismatch\n got %v\nwant %v", got, want)
	}
}

func TestFromBytes_RejectsAShortHeader(t *testing.T) {
	for _, b := range [][]byte{{}, {0x00}} {
		if _, err := FromBytes(b); err == nil {
			t.Errorf("FromBytes(%v) = nil error, want one", b)
		}
	}
}

func TestChunk_TruncatedStreamIsAnErrorNotGarbage(t *testing.T) {
	// A chunk whose header promises more samples than the bytes hold must
	// report an error. Silently returning what it could decode would let a
	// corrupt block look like a short one.
	c := appendAll(t, regular(20, 0, 10000, 1))
	full := c.Bytes()
	for _, cut := range []int{headerLen, headerLen + 1, len(full) / 2, len(full) - 1} {
		truncated, err := FromBytes(append([]byte{}, full[:cut]...))
		if err != nil {
			t.Fatal(err)
		}
		it := truncated.Iterator()
		for it.Next() {
		}
		if it.Err() == nil {
			t.Errorf("truncating to %d bytes decoded cleanly, want an error", cut)
		}
	}
}

func TestChunk_CompressionIsWorthIt(t *testing.T) {
	// The M2 acceptance target is ≤ 2 bytes/sample for a realistic series.
	// This is the encoder's half of that claim, on the two shapes that
	// dominate real data.
	for _, tc := range []struct {
		name    string
		samples []sample
		maxAvg  float64
	}{
		{"counter, 10s steps, steady rate", regular(MaxSamplesPerChunk, 1700000000000, 10000, 1), 2},
		{"gauge, 10s steps, constant", constant(MaxSamplesPerChunk, 42), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := appendAll(t, tc.samples)
			avg := float64(len(c.Bytes())) / float64(len(tc.samples))
			if avg > tc.maxAvg {
				t.Errorf("%.2f bytes/sample, want ≤ %.1f", avg, tc.maxAvg)
			}
			t.Logf("%d samples in %d bytes = %.3f bytes/sample", len(tc.samples), len(c.Bytes()), avg)
		})
	}
}

func TestChunk_ResumingDoesNotWriteIntoTheCallersBytes(t *testing.T) {
	// FromBytes does not copy, so the chunk points at memory someone else
	// owns — today a buffer the block reader allocated, one day a mapped
	// region. Resuming an appender on it and writing must not touch those
	// bytes: the chunk takes its own buffer first.
	src := NewChunk()
	a, err := src.Appender()
	if err != nil {
		t.Fatal(err)
	}
	// Five samples leaves the stream mid-byte, which is the case where a
	// resumed appender continues *inside* the last byte rather than after it.
	for i := 0; i < 5; i++ {
		if err := a.Append(int64(i)*10_000, float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	encoded := append([]byte(nil), src.Bytes()...)

	// The caller's buffer, with spare capacity behind it that an in-place
	// append would scribble into.
	owner := make([]byte, len(encoded), len(encoded)+32)
	copy(owner, encoded)
	spare := owner[:cap(owner)][len(encoded):]
	for i := range spare {
		spare[i] = 0xAA
	}

	c, err := FromBytes(owner)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := c.Appender()
	if err != nil {
		t.Fatal(err)
	}
	for i := 5; i < 40; i++ {
		if err := resumed.Append(int64(i)*10_000, float64(i)); err != nil {
			t.Fatalf("append %d after resume: %v", i, err)
		}
	}

	if !bytes.Equal(owner, encoded) {
		t.Error("resuming rewrote the caller's bytes")
	}
	for i, b := range spare {
		if b != 0xAA {
			t.Fatalf("resuming wrote %#x into byte %d of the caller's spare capacity", b, i)
		}
	}

	// And the chunk itself is still correct, which is the other half.
	it, want := c.Iterator(), 0
	for it.Next() {
		gotT, gotV := it.At()
		if gotT != int64(want)*10_000 || gotV != float64(want) {
			t.Fatalf("sample %d is (%d, %g)", want, gotT, gotV)
		}
		want++
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	if want != 40 {
		t.Errorf("read back %d samples, want 40", want)
	}
}

func TestChunk_AnEmptyBorrowedChunkIsCopiedBeforeTheFirstWrite(t *testing.T) {
	// The empty case of TestChunk_ResumingDoesNotWriteIntoTheCallersBytes,
	// which is not the same code path: there is nothing to resume, so the
	// appender used to be handed back immediately, still pointing at the
	// caller's array. A block's chunk reader hands FromBytes a slice whose
	// spare capacity holds the record's trailing checksum.
	header := []byte{0, 0}
	owner := make([]byte, len(header), len(header)+8)
	copy(owner, header)
	spare := owner[:cap(owner)][len(header):]
	for i := range spare {
		spare[i] = 0xAA
	}

	c, err := FromBytes(owner)
	if err != nil {
		t.Fatal(err)
	}
	if c.NumSamples() != 0 {
		t.Fatalf("fixture holds %d samples, want an empty chunk", c.NumSamples())
	}
	a, err := c.Appender()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		if err := a.Append(int64(i)*10_000, float64(i)); err != nil {
			t.Fatal(err)
		}
	}

	for i, b := range spare {
		if b != 0xAA {
			t.Fatalf("appending wrote %#x into byte %d past the caller's slice", b, i)
		}
	}
	if !bytes.Equal(owner, header) {
		t.Error("appending rewrote the caller's bytes")
	}
	if c.NumSamples() != 20 {
		t.Errorf("chunk holds %d samples, want 20", c.NumSamples())
	}
}

func TestChunk_ABorrowedBufferLongerThanItsPayload(t *testing.T) {
	// The fuzzer's find, written out so the reason survives the corpus file.
	//
	// A chunk's header says how many samples it holds; the bytes behind it are
	// whatever is there. The two disagreeing is one bit-flip away from any real
	// chunk, and the appender's copy sized its new buffer from where the
	// *payload* ends — which, for a header claiming few samples over a long
	// body, is a capacity below the buffer's own length, and make panics on
	// that before anything gets a chance to reject the chunk.
	long := make([]byte, 128)
	long[0], long[1] = 0, 0 // the header: zero samples
	for i := 2; i < len(long); i++ {
		long[i] = '0'
	}
	c, err := FromBytes(long)
	if err != nil {
		t.Fatal(err)
	}
	if c.NumSamples() != 0 {
		t.Fatalf("fixture claims %d samples, want 0", c.NumSamples())
	}
	a, err := c.Appender()
	if err != nil {
		t.Fatal(err) // an error is fine here; a panic is not
	}
	if err := a.Append(1000, 1); err != nil {
		t.Fatalf("append: %v", err)
	}
	// And the caller's bytes are still the caller's.
	for i := 2; i < len(long); i++ {
		if long[i] != '0' {
			t.Fatalf("appending rewrote byte %d of the caller's buffer", i)
		}
	}
}

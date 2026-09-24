package chunkenc

import (
	"bytes"
	"flag"
	"math"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the committed golden files")

// goldenSamples is the fixture behind testdata/golden/chunk.bin. It is chosen
// to exercise every branch of the encoding at once, so a change to any of them
// changes the file: a constant stretch (one bit per sample), a shifted delta
// that needs a bucket, a large gap, a repeated value, a negative, a denormal,
// and a NaN — whose exact bit pattern must survive, because a decoder that
// "helpfully" normalizes it has changed the format.
var goldenSamples = []struct {
	t int64
	v float64
}{
	{1600000000000, 1},
	{1600000010000, 1},
	{1600000020000, 1},
	{1600000030001, 1.5},
	{1600000040001, -1.5},
	{1600000900000, 0},
	{1600000910000, math.Copysign(0, -1)},
	{1600000920000, math.SmallestNonzeroFloat64},
	{1600000930000, math.Float64frombits(0x7FF8000000000001)},
	{1600000940000, math.MaxFloat64},
}

// TestChunk_Golden is the promise that a chunk written by any past version of
// this code is still readable by this one.
//
// Format compatibility is not something unit tests can check, because they
// encode and decode with the same build: a change to both halves at once is
// invisible to them and catastrophic on disk, where the bytes were written
// months ago. The only way to test it is to commit the bytes.
//
// It checks both directions. The committed file must decode to the expected
// samples — that is the compatibility guarantee. And the encoder must still
// produce those exact bytes, which is not required for correctness but turns
// an accidental format change into a failing test instead of a silent one.
// Regenerate deliberately with -update-golden, and when you do, say in the
// commit message why the format changed.
func TestChunk_Golden(t *testing.T) {
	path := filepath.Join("testdata", "golden", "chunk.bin")

	fresh := NewChunk()
	a, err := fresh.Appender()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range goldenSamples {
		if err := a.Append(s.t, s.v); err != nil {
			t.Fatalf("encoding (%d, %g): %v", s.t, s.v, err)
		}
	}
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, fresh.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(fresh.Bytes()))
	}

	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — regenerate with: go test ./internal/tsdb/chunkenc -run Golden -update-golden", err)
	}
	c, err := FromBytes(committed)
	if err != nil {
		t.Fatalf("the committed chunk no longer opens: %v", err)
	}
	if got := c.NumSamples(); got != len(goldenSamples) {
		t.Fatalf("the committed chunk holds %d samples, want %d", got, len(goldenSamples))
	}
	it, i := c.Iterator(), 0
	for it.Next() {
		gotT, gotV := it.At()
		want := goldenSamples[i]
		// Compared as bits: NaN != NaN, and -0 == 0, and both of those are
		// exactly the cases this fixture exists to pin down.
		if gotT != want.t || math.Float64bits(gotV) != math.Float64bits(want.v) {
			t.Errorf("sample %d decoded as (%d, %x), want (%d, %x)",
				i, gotT, math.Float64bits(gotV), want.t, math.Float64bits(want.v))
		}
		i++
	}
	if err := it.Err(); err != nil {
		t.Fatalf("decoding the committed chunk: %v", err)
	}
	if i != len(goldenSamples) {
		t.Errorf("decoded %d samples, want %d", i, len(goldenSamples))
	}
	if !bytes.Equal(fresh.Bytes(), committed) {
		t.Errorf("the encoder no longer produces the committed bytes (%d now, %d committed).\n"+
			"If that is intended, regenerate with -update-golden and explain the format change.",
			len(fresh.Bytes()), len(committed))
	}
}

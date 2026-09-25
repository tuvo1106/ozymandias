package sketchstore

import (
	"bytes"
	"errors"
	"flag"
	"math"
	"os"
	"path/filepath"
	"testing"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/sketch"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite the committed golden files")

// A stored sketch must come back answering every question identically.
// Anything less and a percentile depends on whether the answer came from the
// head of the intake path or from disk.
func TestProperty_ValueRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := rapid.IntRange(1, 200).Draw(t, "n")
		s := sketch.NewDefault()
		for i := 0; i < n; i++ {
			exp := rapid.IntRange(-6, 9).Draw(t, "exp")
			mant := rapid.Float64Range(1, 10).Draw(t, "mant")
			v := mant * math.Pow(10, float64(exp))
			if rapid.Bool().Draw(t, "negative") {
				v = -v
			}
			// A fractional weight is the sampled case, and the one that
			// decides whether the counts can travel as varints.
			w := 1.0
			if rapid.Bool().Draw(t, "sampled") {
				w = rapid.Float64Range(0.1, 5).Draw(t, "weight")
			}
			if err := s.AddWithCount(v, w); err != nil {
				t.Fatalf("AddWithCount(%v, %v): %v", v, w, err)
			}
		}

		encoded, err := encodeValue(s)
		if err != nil {
			t.Fatalf("encodeValue: %v", err)
		}
		got, err := decodeValue(encoded)
		if err != nil {
			t.Fatalf("decodeValue: %v", err)
		}
		if got.Count() != s.Count() || got.Sum() != s.Sum() || got.Min() != s.Min() ||
			got.Max() != s.Max() || got.ZeroCount() != s.ZeroCount() || got.Gamma() != s.Gamma() {
			t.Fatalf("aggregates differ:\n got %+v\nwant %+v", got.ToWire(), s.ToWire())
		}
		for _, q := range []float64{0, 0.25, 0.5, 0.95, 0.99, 1} {
			a, _ := s.Quantile(q)
			b, _ := got.Quantile(q)
			if a != b {
				t.Errorf("q=%v: got %v, want %v", q, b, a)
			}
		}
	})
}

// Whole counts travel as varints and fractional ones as float64s. The flag is
// per store rather than per bucket, so this is what says the common case
// actually takes the cheap path.
func TestEncode_WholeCountsAreCheaperThanFractionalOnes(t *testing.T) {
	whole, fractional := sketch.NewDefault(), sketch.NewDefault()
	for i := 1; i <= 300; i++ {
		v := float64(i)
		if err := whole.Add(v); err != nil {
			t.Fatal(err)
		}
		if err := fractional.AddWithCount(v, 1.5); err != nil {
			t.Fatal(err)
		}
	}
	a, err := encodePayload(whole)
	if err != nil {
		t.Fatal(err)
	}
	b, err := encodePayload(fractional)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) >= len(b) {
		t.Errorf("whole counts encoded to %d bytes and fractional ones to %d; "+
			"whole counts should be varints and fractional ones float64s", len(a), len(b))
	}
}

// A payload too small for zstd to pay for itself is stored raw. The codec
// byte says which, so a reader never has to guess.
func TestEncode_SmallPayloadsAreStoredRaw(t *testing.T) {
	small := sketch.NewDefault()
	if err := small.Add(1); err != nil {
		t.Fatal(err)
	}
	v, err := encodeValue(small)
	if err != nil {
		t.Fatal(err)
	}
	if v[0] != codecRaw {
		t.Errorf("a one-bucket sketch was compressed (codec %d); the frame header costs more than it saves", v[0])
	}

	big := sketch.NewDefault()
	for i := 1; i <= 2000; i++ {
		if err := big.Add(float64(i)); err != nil {
			t.Fatal(err)
		}
	}
	v, err = encodeValue(big)
	if err != nil {
		t.Fatal(err)
	}
	if v[0] != codecZstd {
		t.Errorf("a 2000-observation sketch was stored raw (codec %d)", v[0])
	}
	raw, err := encodePayload(big)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) >= len(raw) {
		t.Errorf("compression made it bigger: %d stored vs %d raw", len(v), len(raw))
	}
}

// Every length decodeValue reads comes off a disk this process does not own.
// A corrupt one must be an error, never an allocation and never a panic.
func TestDecode_RejectsCorruptValues(t *testing.T) {
	good := sketch.NewDefault()
	for i := range 10 {
		if err := good.Add(float64(i + 1)); err != nil {
			t.Fatal(err)
		}
	}
	valid, err := encodeValue(good)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		v    []byte
	}{
		{"empty", nil},
		{"unknown codec", []byte{9, 1, 2, 3}},
		{"zstd that is not zstd", []byte{codecZstd, 1, 2, 3}},
		{"raw but too short for a header", []byte{codecRaw, formatVersion, 0, 0}},
		{"wrong payload version", func() []byte {
			v := bytes.Clone(valid)
			v[1] = formatVersion + 1
			return v
		}()},
		{"truncated mid-bucket", valid[:len(valid)-3]},
		{"trailing bytes", append(bytes.Clone(valid), 0, 0, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeValue(tc.v)
			if err == nil {
				t.Fatalf("accepted a corrupt value: %+v", got.ToWire())
			}
			if !errors.Is(err, errCorrupt) {
				t.Errorf("error %v does not wrap errCorrupt", err)
			}
		})
	}
}

// A bucket count read off disk is a number this process was told, not one it
// computed. Claiming more buckets than the store can hold must be refused
// before anything is allocated for them.
func TestDecode_RefusesAnImpossibleBucketCount(t *testing.T) {
	// A hand-built payload: version, six aggregates, then a bucket run
	// claiming 2^40 buckets.
	p := make([]byte, 0, 64)
	p = append(p, formatVersion)
	for _, f := range []float64{1.02, 0, 0, 0, 0, 0} {
		p = binaryAppendFloat(p, f)
	}
	p = append(p, countsIntegral)
	p = appendUvarint(p, 1<<40)
	if _, err := decodeValue(append([]byte{codecRaw}, p...)); err == nil {
		t.Fatal("accepted a value claiming 2^40 buckets")
	} else if !errors.Is(err, errCorrupt) {
		t.Errorf("error %v does not wrap errCorrupt", err)
	}
}

// TestSketchValue_Golden is the promise that a sketch written by any past
// version of this code still decodes on this one.
//
// These bytes are what a Pebble value holds after a restart, an upgrade or a
// backup restore — all moments when discovering that the layout moved is far
// too late. A round-trip test cannot see it, because it encodes and decodes
// with the same build; only committed bytes can. Regenerate with
// -update-golden, and treat needing to as the news that it is.
func TestSketchValue_Golden(t *testing.T) {
	path := filepath.Join("testdata", "golden", "sketch-v1.bin")

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		v, err := encodeValue(goldenSketch(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, v, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d bytes)", path, len(v))
	}

	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — regenerate with: go test ./internal/sketchstore -run Golden -update-golden", err)
	}
	got, err := decodeValue(stored)
	if err != nil {
		t.Fatalf("the committed sketch no longer decodes: %v", err)
	}
	want := goldenSketch(t)
	if got.Count() != want.Count() || got.Sum() != want.Sum() ||
		got.Min() != want.Min() || got.Max() != want.Max() ||
		got.ZeroCount() != want.ZeroCount() || got.Gamma() != want.Gamma() {
		t.Fatalf("the committed sketch decodes to different aggregates:\n got %+v\nwant %+v",
			got.ToWire(), want.ToWire())
	}
	for _, q := range []float64{0, 0.5, 0.9, 0.99, 1} {
		a, _ := want.Quantile(q)
		b, _ := got.Quantile(q)
		if a != b {
			t.Errorf("q=%v: the committed sketch answers %v, this build's answers %v", q, b, a)
		}
	}

	// And the bytes themselves, so a change in the *encoder* is also news
	// rather than something only a future reader finds out about.
	fresh, err := encodeValue(want)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fresh, stored) {
		t.Errorf("this build encodes the fixture differently than the committed file.\n"+
			"If that is intended, regenerate with -update-golden and explain the format change.\n"+
			"got  %d bytes\nwant %d bytes", len(fresh), len(stored))
	}
}

// goldenSketch is the fixture behind testdata/golden/sketch-v1.bin: positive
// and negative values, a zero, and a fractional weight, because those are the
// four things a layout change gets wrong.
func goldenSketch(t *testing.T) *sketch.Sketch {
	t.Helper()
	s := sketch.NewDefault()
	add := func(v, w float64) {
		t.Helper()
		if err := s.AddWithCount(v, w); err != nil {
			t.Fatalf("AddWithCount(%v, %v): %v", v, w, err)
		}
	}
	for i := 1; i <= 40; i++ {
		add(float64(i), 1)
	}
	add(-17.5, 1)
	add(-1e6, 1)
	add(0, 1)
	add(96.25, 2.5)
	return s
}

func FuzzDecodeValue(f *testing.F) {
	s := sketch.NewDefault()
	for i := range 20 {
		_ = s.Add(float64(i + 1))
	}
	if v, err := encodeValue(s); err == nil {
		f.Add(v)
	}
	f.Add([]byte{codecRaw, formatVersion})
	f.Add([]byte{codecZstd})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, v []byte) {
		got, err := decodeValue(v)
		if err != nil {
			return
		}
		// Whatever survives must be a usable sketch: re-encoding it and
		// reading it back has to give the same answers, or the decoder has
		// accepted something it cannot represent.
		again, err := encodeValue(got)
		if err != nil {
			t.Fatalf("a decoded sketch does not re-encode: %v", err)
		}
		back, err := decodeValue(again)
		if err != nil {
			t.Fatalf("a re-encoded sketch does not decode: %v", err)
		}
		if back.Count() != got.Count() || back.Sum() != got.Sum() {
			t.Fatalf("round trip changed the sketch: %+v vs %+v", back.ToWire(), got.ToWire())
		}
	})
}

// Small helpers so the hand-built corrupt payloads above read as bytes rather
// than as calls into encoding/binary.
func binaryAppendFloat(b []byte, f float64) []byte {
	var out [8]byte
	bits := math.Float64bits(f)
	for i := range out {
		out[i] = byte(bits >> (8 * i))
	}
	return append(b, out[:]...)
}

func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

// The remaining ways a bucket run can be wrong, each hand-built because no
// encoder produces them.
func TestDecode_RejectsMalformedBucketRuns(t *testing.T) {
	header := func() []byte {
		p := []byte{formatVersion}
		for _, f := range []float64{1.02, 0, 0, 0, 0, 0} {
			p = binaryAppendFloat(p, f)
		}
		return p
	}
	for _, tc := range []struct {
		name string
		tail []byte
	}{
		{"no bucket run at all", nil},
		{"unknown bucket flags", []byte{0x80}},
		{"unreadable bucket count", []byte{countsIntegral, 0xff}},
		{"truncated index", append([]byte{countsIntegral}, appendUvarint(nil, 1)...)},
		{"truncated float count", func() []byte {
			b := []byte{countsFloat}
			b = appendUvarint(b, 1)
			b = append(b, 2) // zigzag varint for index 1
			return append(b, 0, 0, 0)
		}()},
		{"index outside int32", func() []byte {
			b := []byte{countsIntegral}
			b = appendUvarint(b, 1)
			b = appendUvarint(b, 1<<33) // zigzag of a huge index
			return b
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := append([]byte{codecRaw}, append(header(), tc.tail...)...)
			if got, err := decodeValue(v); err == nil {
				t.Fatalf("accepted a malformed bucket run: %+v", got.ToWire())
			} else if !errors.Is(err, errCorrupt) {
				t.Errorf("error %v does not wrap errCorrupt", err)
			}
		})
	}
}

// Encoding nothing is a programming error, not a payload.
func TestEncode_NilSketch(t *testing.T) {
	if _, err := encodeValue(nil); err == nil {
		t.Fatal("encoded a nil sketch")
	}
}

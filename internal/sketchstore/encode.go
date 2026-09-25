package sketchstore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// formatVersion is the payload layout's version, written as its first byte.
// docs/formats/sketch.md is the byte-level spec.
const formatVersion = 1

// Codecs, written as the stored value's first byte — outside the payload, so
// reading one does not require decompressing the other.
const (
	codecRaw  = 0
	codecZstd = 1
)

// Count encodings, a flag per store rather than per bucket.
//
// Bucket counts are float64 because a sampled metric contributes 1/rate per
// observation, but the overwhelming majority of sketches never see a sample
// rate and hold whole numbers. A per-bucket discriminator would cost a byte
// on every bucket to say so; a per-store flag costs one byte for all of them.
const (
	countsFloat    = 0
	countsIntegral = 1 << 0
)

// maxDecompressedValue bounds what one stored value may expand to. A full
// sketch — 2048 buckets each side, with float counts — is about 41 KiB, so
// this is roomy by a factor of 25 and still refuses a value that claims to
// expand to a gigabyte.
const maxDecompressedValue = 1 << 20

// compressionFloor is the size below which a payload is stored raw. zstd's
// frame header is 9–18 bytes and small sketches do not recover it; storing
// them raw also keeps the common single-bucket sketch readable in a hex dump.
const compressionFloor = 128

var errCorrupt = errors.New("sketchstore: corrupt value")

// encodeValue renders a sketch as one stored value: a codec byte, then the
// payload or its zstd frame.
func encodeValue(s *sketch.Sketch) ([]byte, error) {
	payload, err := encodePayload(s)
	if err != nil {
		return nil, err
	}
	if len(payload) < compressionFloor {
		return append([]byte{codecRaw}, payload...), nil
	}
	compressed := encoderPool().EncodeAll(payload, make([]byte, 0, len(payload)/2))
	// Incompressible data comes back larger than it went in. Storing that
	// would cost space *and* a decompression on every read.
	if len(compressed) >= len(payload) {
		return append([]byte{codecRaw}, payload...), nil
	}
	return append([]byte{codecZstd}, compressed...), nil
}

// decodeValue rebuilds a sketch from a stored value. Every length it reads
// comes from bytes on disk, so every one of them is checked: a corrupt value
// must be an error, never an allocation.
func decodeValue(v []byte) (*sketch.Sketch, error) {
	if len(v) == 0 {
		return nil, fmt.Errorf("%w: empty", errCorrupt)
	}
	payload := v[1:]
	switch v[0] {
	case codecRaw:
	case codecZstd:
		out, err := decoderPool().DecodeAll(payload, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: zstd: %w", errCorrupt, err)
		}
		payload = out
	default:
		return nil, fmt.Errorf("%w: unknown codec %d", errCorrupt, v[0])
	}
	return decodePayload(payload)
}

// encodePayload writes the sketch itself: a version, the six exact
// aggregates, then each store's buckets as index deltas and counts.
//
// The aggregates are full float64s because they are exact values the query
// layer reports as-is — count, sum, min and max are not estimates, and
// squeezing them would make them so. The buckets are varints because that is
// where the bytes are: a few hundred of them, whose indices are a dense
// ascending run.
func encodePayload(s *sketch.Sketch) ([]byte, error) {
	if s == nil {
		return nil, errors.New("sketchstore: nil sketch")
	}
	w := s.ToWire()
	buf := make([]byte, 0, 64+6*len(w.Bins)+6*len(w.NegBins))
	buf = append(buf, formatVersion)
	for _, f := range [...]float64{w.Gamma, w.Count, w.Sum, w.Min, w.Max, w.Zeros} {
		buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(f))
	}
	buf = appendBins(buf, w.Bins)
	return appendBins(buf, w.NegBins), nil
}

func appendBins(buf []byte, bins []wire.SketchBin) []byte {
	flags := byte(countsIntegral)
	for _, b := range bins {
		if b.Count != math.Trunc(b.Count) || b.Count < 0 || b.Count >= math.MaxUint64 {
			flags = countsFloat
			break
		}
	}
	buf = append(buf, flags)
	buf = binary.AppendUvarint(buf, uint64(len(bins)))
	prev := 0
	for _, b := range bins {
		buf = binary.AppendVarint(buf, int64(b.Index-prev))
		prev = b.Index
		if flags&countsIntegral != 0 {
			buf = binary.AppendUvarint(buf, uint64(b.Count))
		} else {
			buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(b.Count))
		}
	}
	return buf
}

func decodePayload(p []byte) (*sketch.Sketch, error) {
	if len(p) < 1+6*8 {
		return nil, fmt.Errorf("%w: %d bytes, too short for a header", errCorrupt, len(p))
	}
	if p[0] != formatVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", errCorrupt, p[0], formatVersion)
	}
	p = p[1:]
	var agg [6]float64
	for i := range agg {
		agg[i] = math.Float64frombits(binary.LittleEndian.Uint64(p[i*8:]))
	}
	p = p[6*8:]

	pos, p, err := readBins(p)
	if err != nil {
		return nil, err
	}
	neg, p, err := readBins(p)
	if err != nil {
		return nil, err
	}
	if len(p) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes", errCorrupt, len(p))
	}

	// Through the same door a payload off the network uses: the bytes came
	// off a disk this process does not own either, and the sketch's own
	// decoder is the thing that knows what it can represent.
	w := wire.Sketch{
		Gamma: agg[0], Count: agg[1], Sum: agg[2], Min: agg[3], Max: agg[4], Zeros: agg[5],
		Bins: pos, NegBins: neg,
	}
	s, err := sketch.FromWire(w)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errCorrupt, err)
	}
	return s, nil
}

func readBins(p []byte) ([]wire.SketchBin, []byte, error) {
	if len(p) < 1 {
		return nil, nil, fmt.Errorf("%w: truncated before a bucket run", errCorrupt)
	}
	flags := p[0]
	if flags&^countsIntegral != 0 {
		return nil, nil, fmt.Errorf("%w: unknown bucket flags %#x", errCorrupt, flags)
	}
	p = p[1:]
	n, used := binary.Uvarint(p)
	if used <= 0 {
		return nil, nil, fmt.Errorf("%w: unreadable bucket count", errCorrupt)
	}
	// Checked before it is used to allocate: the number is on disk, and a
	// corrupt one claiming 2^60 buckets must not be believed for as long as
	// it takes to call make.
	if n > sketch.MaxBins {
		return nil, nil, fmt.Errorf("%w: %d buckets, the limit is %d", errCorrupt, n, sketch.MaxBins)
	}
	p = p[used:]
	bins := make([]wire.SketchBin, 0, n)
	prev := int64(0)
	for i := uint64(0); i < n; i++ {
		delta, used := binary.Varint(p)
		if used <= 0 {
			return nil, nil, fmt.Errorf("%w: unreadable bucket index at %d", errCorrupt, i)
		}
		p = p[used:]
		idx := prev + delta
		if idx < math.MinInt32 || idx > math.MaxInt32 {
			return nil, nil, fmt.Errorf("%w: bucket index %d outside ±2^31", errCorrupt, idx)
		}
		prev = idx

		var count float64
		if flags&countsIntegral != 0 {
			c, used := binary.Uvarint(p)
			if used <= 0 {
				return nil, nil, fmt.Errorf("%w: unreadable bucket count at %d", errCorrupt, i)
			}
			p, count = p[used:], float64(c)
		} else {
			if len(p) < 8 {
				return nil, nil, fmt.Errorf("%w: truncated bucket count at %d", errCorrupt, i)
			}
			count = math.Float64frombits(binary.LittleEndian.Uint64(p))
			p = p[8:]
		}
		bins = append(bins, wire.SketchBin{Index: int(idx), Count: count})
	}
	return bins, p, nil
}

// Encoder and decoder pools. zstd's encoder holds megabytes of window state,
// so one per call would allocate more than the compression saves.
var (
	encOnce sync.Once
	enc     *zstd.Encoder
	decOnce sync.Once
	dec     *zstd.Decoder
)

func encoderPool() *zstd.Encoder {
	encOnce.Do(func() {
		// SpeedDefault, not SpeedBestCompression: this runs on the intake
		// path, once per sketch per bucket, and the payload is a kilobyte.
		e, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			panic("sketchstore: zstd encoder: " + err.Error())
		}
		enc = e
	})
	return enc
}

func decoderPool() *zstd.Decoder {
	decOnce.Do(func() {
		d, err := zstd.NewReader(nil,
			zstd.WithDecoderConcurrency(0),
			zstd.WithDecoderMaxMemory(maxDecompressedValue))
		if err != nil {
			panic("sketchstore: zstd decoder: " + err.Error())
		}
		dec = d
	})
	return dec
}

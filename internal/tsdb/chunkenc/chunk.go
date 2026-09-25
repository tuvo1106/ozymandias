package chunkenc

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
)

// MaxSamplesPerChunk is where a chunk is cut. Chunks are read by replaying
// the bitstream from the start, so the cap bounds the work an iterator does
// to reach the last sample — and bounds how much is re-read when a query
// touches only the tail of a chunk.
const MaxSamplesPerChunk = 120

// headerLen is the uint16 sample count that prefixes every chunk.
const headerLen = 2

// Chunk is a compressed, append-only run of samples from one series.
//
// The zero Chunk is a valid empty chunk. Bytes returns the encoding, which is
// what a block writes to disk; FromBytes wraps such an encoding for reading.
// A Chunk being read must not be appended to.
type Chunk struct {
	b bstream
	// borrowed records that the bytes came from [FromBytes] and belong to
	// somebody else. An appender must take its own copy before writing.
	borrowed bool
}

// NewChunk returns an empty chunk ready for an Appender.
func NewChunk() *Chunk {
	return &Chunk{b: bstream{stream: make([]byte, headerLen, 64)}}
}

// FromBytes wraps an existing encoding. The slice is not copied and must not
// be modified while the chunk is in use.
//
// The chunk remembers that the bytes are not its own, so that appending to it
// — which is legal, and is how a chunk read back from disk is resumed — copies
// first instead of writing into the caller's buffer. See [Chunk.Appender].
func FromBytes(b []byte) (*Chunk, error) {
	if len(b) < headerLen {
		return nil, fmt.Errorf("chunkenc: chunk of %d bytes is shorter than its header", len(b))
	}
	return &Chunk{b: bstream{stream: b}, borrowed: true}, nil
}

// Bytes returns the chunk's encoding. The result aliases the chunk's buffer.
func (c *Chunk) Bytes() []byte { return c.b.bytes() }

// NumSamples reports how many samples the chunk holds.
func (c *Chunk) NumSamples() int {
	if len(c.b.stream) < headerLen {
		return 0
	}
	return int(binary.BigEndian.Uint16(c.b.stream))
}

// Full reports whether the chunk has reached MaxSamplesPerChunk.
func (c *Chunk) Full() bool { return c.NumSamples() >= MaxSamplesPerChunk }

// Appender adds samples to a chunk. One appender per chunk; it holds the
// encoder state (previous timestamp, delta, value and XOR window) that the
// next sample is encoded against.
type Appender struct {
	c *Chunk

	t     int64
	v     float64
	delta int64

	leading  uint8 // leading zeros of the previous XOR, 0xff = "no window yet"
	trailing uint8
}

// Appender returns an appender positioned at the end of the chunk. Appending
// to a chunk decoded with FromBytes requires replaying it, so this returns an
// error for a non-empty chunk it cannot resume.
func (c *Chunk) Appender() (*Appender, error) {
	a := &Appender{c: c, leading: 0xff}
	n := c.NumSamples()
	if n == 0 {
		// Even with nothing to resume, borrowed bytes have to be copied before
		// the first write: an empty chunk read out of a block sits in a buffer
		// whose spare capacity is somebody else's data — the record's trailing
		// checksum, as it happens — and appending would write straight over it.
		c.own(headerLen)
		return a, nil
	}
	// Resume: replay to recover the encoder state. The alternative — storing
	// the state alongside the chunk — would be a second source of truth that
	// can disagree with the bytes.
	it := c.Iterator()
	for it.Next() {
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("chunkenc: cannot resume chunk: %w", err)
	}
	a.t, a.v, a.delta = it.t, it.v, it.delta
	a.leading, a.trailing = it.leading, it.trailing
	// Restore the *bit* cursor, not just the byte one. FromBytes has no idea
	// how much of the final byte is payload, and a writer that assumes a byte
	// boundary would leave a hole of padding bits in the middle of the
	// stream — which decodes as garbage from that sample on, silently.
	//
	// The reader's count is how many bits of stream[pos] it has *not* read. A
	// full 8 means it never touched that byte, so the payload ends at pos-1
	// and the writer needs a fresh byte; anything less means pos is the
	// partially-filled byte the writer must continue in, and the unread bits
	// are exactly the free ones.
	//
	// Keeping the byte at pos unconditionally, as this once did, appends a
	// stray byte to every resumed chunk.
	end, count := headerLen+it.br.pos, uint8(0)
	if it.br.count != 8 {
		end, count = end+1, it.br.count
	}
	// From here the appender writes into the chunk's buffer, so if that buffer
	// is on loan it has to be replaced first. [FromBytes] does not copy: the
	// bytes may be a block's file buffer, one day a mapped region, and writing
	// past the truncation point would corrupt their data rather than ours.
	//
	// Owning the bytes also makes the shape of an old bug impossible rather
	// than merely fixed: reslicing to end+1 panics outright when the backing
	// array has no spare capacity, which is exactly what a chunk read from a
	// block has.
	a.c.own(end)
	a.c.b.stream, a.c.b.count = a.c.b.stream[:end], count
	return a, nil
}

// own replaces a borrowed buffer with one the chunk can write to, with room to
// grow past n bytes. A chunk that already owns its bytes is left alone, so the
// ordinary path — a chunk this package allocated, resumed in place — costs
// nothing.
//
// The whole stream is copied, not the first n bytes, and the capacity is taken
// from whichever is larger. n is where the *payload* ends, and a chunk read
// off a disk can be longer than that: a header claiming fewer samples than the
// bytes behind it is one edit away from any real chunk, and sizing the new
// buffer from n alone asks for a capacity below its own length, which panics.
// Found by the fuzzer, in this function, the day it was written.
func (c *Chunk) own(n int) {
	if !c.borrowed {
		return
	}
	if n < len(c.b.stream) {
		n = len(c.b.stream)
	}
	buf := make([]byte, len(c.b.stream), n+64)
	copy(buf, c.b.stream)
	c.b.stream = buf
	c.borrowed = false
}

// Append adds one sample. Timestamps must strictly increase: an out-of-order
// or duplicate timestamp is a programming error at this layer (the head
// enforces the rule with a real policy) and is rejected rather than encoded,
// because the delta-of-delta scheme cannot represent going backwards.
func (a *Appender) Append(t int64, v float64) error {
	n := a.c.NumSamples()
	if n >= MaxSamplesPerChunk {
		return fmt.Errorf("chunkenc: chunk is full at %d samples", MaxSamplesPerChunk)
	}
	switch n {
	case 0:
		a.c.b.writeVarint(t)
		a.c.b.writeBits(math.Float64bits(v), 64)
	case 1:
		if t <= a.t {
			return fmt.Errorf("chunkenc: timestamp %d is not after %d", t, a.t)
		}
		a.delta = t - a.t
		a.c.b.writeVarint(a.delta)
		a.writeValue(v)
	default:
		if t <= a.t {
			return fmt.Errorf("chunkenc: timestamp %d is not after %d", t, a.t)
		}
		delta := t - a.t
		a.writeDOD(delta - a.delta)
		a.delta = delta
		a.writeValue(v)
	}
	a.t, a.v = t, v
	binary.BigEndian.PutUint16(a.c.b.stream, uint16(n+1))
	return nil
}

// dodBuckets are the delta-of-delta size classes: prefix, prefix length, and
// how many bits of payload follow. A regular series always takes the first.
// The ranges are Prometheus's rather than the paper's, because millisecond
// timestamps need more room than seconds.
var dodBuckets = []struct {
	prefix     uint64
	prefixBits int
	bits       int // payload bits, 0 for the "no change" bucket
}{
	{0b0, 1, 0},
	{0b10, 2, 14},
	{0b110, 3, 17},
	{0b1110, 4, 20},
	{0b1111, 4, 64},
}

func (a *Appender) writeDOD(dod int64) {
	for _, b := range dodBuckets {
		if b.bits == 0 {
			if dod == 0 {
				a.c.b.writeBits(b.prefix, b.prefixBits)
				return
			}
			continue
		}
		// A signed value of n bits spans [-2^(n-1), 2^(n-1)-1].
		if b.bits < 64 && (dod < -(1<<(b.bits-1)) || dod > (1<<(b.bits-1))-1) {
			continue
		}
		a.c.b.writeBits(b.prefix, b.prefixBits)
		a.c.b.writeBits(uint64(dod), b.bits)
		return
	}
}

// writeValue XORs against the previous value and stores only the bits that
// changed. Reusing the previous leading/trailing window when the new XOR fits
// inside it saves the 11 bits of window description — which is most samples,
// because a series' values tend to stay in the same magnitude.
func (a *Appender) writeValue(v float64) {
	x := math.Float64bits(v) ^ math.Float64bits(a.v)
	if x == 0 {
		a.c.b.writeBit(false)
		return
	}
	a.c.b.writeBit(true)

	leading := uint8(bits.LeadingZeros64(x))
	trailing := uint8(bits.TrailingZeros64(x))
	// 5 bits hold at most 31, so clamp; the cost is a few wasted payload bits.
	if leading >= 32 {
		leading = 31
	}
	if a.leading != 0xff && leading >= a.leading && trailing >= a.trailing {
		a.c.b.writeBit(false) // reuse the previous window
		a.c.b.writeBits(x>>a.trailing, 64-int(a.leading)-int(a.trailing))
		return
	}
	a.c.b.writeBit(true)
	a.c.b.writeBits(uint64(leading), 5)
	// A window is 1..64 bits wide; 64 is written as 0 because 6 bits cannot
	// hold 64. Width 0 cannot occur here: x != 0.
	sigbits := 64 - int(leading) - int(trailing)
	a.c.b.writeBits(uint64(sigbits)&0x3f, 6)
	a.c.b.writeBits(x>>trailing, sigbits)
	a.leading, a.trailing = leading, trailing
}

// Iterator walks a chunk's samples in order.
type Iterator struct {
	br       *bstreamReader
	total    int
	read     int
	t        int64
	v        float64
	delta    int64
	leading  uint8
	trailing uint8
	err      error
}

// Iterator returns an iterator over the chunk's samples.
func (c *Chunk) Iterator() *Iterator {
	if len(c.b.stream) < headerLen {
		return &Iterator{br: newBReader(nil), err: ErrEndOfStream}
	}
	return &Iterator{
		br:      newBReader(c.b.stream[headerLen:]),
		total:   c.NumSamples(),
		leading: 0xff,
	}
}

// At returns the current sample. Only valid after Next reported true.
func (it *Iterator) At() (int64, float64) { return it.t, it.v }

// Err returns the first decoding error, if any. A well-formed chunk ends with
// Next returning false and Err nil; a truncated or corrupt one reports here.
func (it *Iterator) Err() error { return it.err }

// Next advances to the next sample.
func (it *Iterator) Next() bool {
	if it.err != nil || it.read >= it.total {
		return false
	}
	switch it.read {
	case 0:
		t, err := it.br.readVarint()
		if err != nil {
			return it.fail(err)
		}
		v, err := it.br.readBits(64)
		if err != nil {
			return it.fail(err)
		}
		it.t, it.v = t, math.Float64frombits(v)
	case 1:
		d, err := it.br.readVarint()
		if err != nil {
			return it.fail(err)
		}
		it.delta = d
		it.t += d
		if !it.readValue() {
			return false
		}
	default:
		dod, ok := it.readDOD()
		if !ok {
			return false
		}
		it.delta += dod
		it.t += it.delta
		if !it.readValue() {
			return false
		}
	}
	it.read++
	return true
}

func (it *Iterator) readDOD() (int64, bool) {
	var prefix int
	for prefix < 4 {
		bit, err := it.br.readBit()
		if err != nil {
			it.fail(err)
			return 0, false
		}
		if !bit {
			break
		}
		prefix++
	}
	var nbits int
	switch prefix {
	case 0:
		return 0, true
	case 1:
		nbits = 14
	case 2:
		nbits = 17
	case 3:
		nbits = 20
	default:
		nbits = 64
	}
	u, err := it.br.readBits(nbits)
	if err != nil {
		it.fail(err)
		return 0, false
	}
	// Sign-extend: the payload is a two's-complement value of nbits.
	if nbits < 64 && u&(1<<(nbits-1)) != 0 {
		u |= ^uint64(0) << nbits
	}
	return int64(u), true
}

func (it *Iterator) readValue() bool {
	changed, err := it.br.readBit()
	if err != nil {
		return it.fail(err)
	}
	if !changed {
		return true // identical to the previous value
	}
	newWindow, err := it.br.readBit()
	if err != nil {
		return it.fail(err)
	}
	if newWindow {
		leading, err := it.br.readBits(5)
		if err != nil {
			return it.fail(err)
		}
		sigbits, err := it.br.readBits(6)
		if err != nil {
			return it.fail(err)
		}
		if sigbits == 0 {
			sigbits = 64 // 6 bits cannot hold 64; the writer stores it as 0
		}
		it.leading = uint8(leading)
		it.trailing = uint8(64 - leading - sigbits)
	}
	if it.leading == 0xff {
		// A reuse bit before any window was established: corrupt input.
		return it.fail(fmt.Errorf("chunkenc: value window reused before it was set"))
	}
	sigbits := 64 - int(it.leading) - int(it.trailing)
	if sigbits <= 0 || sigbits > 64 {
		return it.fail(fmt.Errorf("chunkenc: impossible value window of %d bits", sigbits))
	}
	u, err := it.br.readBits(sigbits)
	if err != nil {
		return it.fail(err)
	}
	it.v = math.Float64frombits(math.Float64bits(it.v) ^ (u << it.trailing))
	return true
}

func (it *Iterator) fail(err error) bool {
	it.err = err
	return false
}

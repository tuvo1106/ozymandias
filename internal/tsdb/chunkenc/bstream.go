package chunkenc

import "errors"

// ErrEndOfStream is returned by a reader that has run out of bits. It means
// the stream ended where a value was expected — either a truncated chunk or a
// sample count that promises more than the bytes hold.
var ErrEndOfStream = errors.New("chunkenc: end of bitstream")

// bstream is an append-only bit writer over a growing byte slice.
//
// Bits are written most-significant-first within each byte, so a hex dump of
// a chunk reads left to right in the same order as the format spec. count is
// how many bits remain free in the last byte, which makes the common
// "append one bit" path a shift and an OR.
type bstream struct {
	stream []byte
	count  uint8 // free bits in the last byte, 0 when a new byte is needed
}

func (b *bstream) bytes() []byte { return b.stream }

func (b *bstream) writeBit(bit bool) {
	if b.count == 0 {
		b.stream = append(b.stream, 0)
		b.count = 8
	}
	if bit {
		b.stream[len(b.stream)-1] |= 1 << (b.count - 1)
	}
	b.count--
}

// writeBits writes the low nbits of u, most significant first.
func (b *bstream) writeBits(u uint64, nbits int) {
	if nbits <= 0 {
		return
	}
	if nbits < 64 {
		u <<= 64 - uint(nbits) // left-align, so the next shift picks the top bit
	}
	for nbits >= 8 && b.count == 0 {
		// Whole aligned byte: skip the per-bit loop entirely. This is the case
		// that makes writing a raw float64 cheap.
		//
		// The condition is count == 0, "the last byte is full, so the next bit
		// starts a new one". It used to read count == 8, which never holds
		// here: count is the *free* bits in the last byte, and writeBit sets
		// it to 8 and decrements it in the same breath, so on entry it is
		// always in [0,7]. The fast path was dead and every bit went the slow
		// way, including the 64-bit raw float this exists for. The output is
		// identical either way, which is why the golden chunk does not move.
		b.stream = append(b.stream, byte(u>>56))
		u <<= 8
		nbits -= 8
	}
	for ; nbits > 0; nbits-- {
		b.writeBit(u&(1<<63) != 0)
		u <<= 1
	}
}

func (b *bstream) writeByte(v byte) { b.writeBits(uint64(v), 8) }

// writeVarint writes a signed integer in protobuf zig-zag varint form: small
// magnitudes, positive or negative, cost one byte.
func (b *bstream) writeVarint(v int64) {
	u := uint64(v) << 1
	if v < 0 {
		u = ^u
	}
	for u >= 0x80 {
		b.writeByte(byte(u) | 0x80)
		u >>= 7
	}
	b.writeByte(byte(u))
}

// bstreamReader replays a bstream. It holds the remaining bytes and how far
// into the first one it has read; nothing is mutated but the cursor.
type bstreamReader struct {
	stream []byte
	pos    int   // index of the byte being read
	count  uint8 // unread bits in stream[pos], 8 when none has been consumed
}

func newBReader(b []byte) *bstreamReader {
	return &bstreamReader{stream: b, count: 8}
}

func (b *bstreamReader) readBit() (bool, error) {
	if b.pos >= len(b.stream) {
		return false, ErrEndOfStream
	}
	if b.count == 0 {
		b.pos++
		if b.pos >= len(b.stream) {
			return false, ErrEndOfStream
		}
		b.count = 8
	}
	b.count--
	return b.stream[b.pos]&(1<<b.count) != 0, nil
}

// readBits reads nbits, most significant first, into the low bits of the
// result. nbits must be in [0, 64].
func (b *bstreamReader) readBits(nbits int) (uint64, error) {
	var u uint64
	for ; nbits >= 8 && b.count == 8; nbits -= 8 {
		if b.pos >= len(b.stream) {
			return 0, ErrEndOfStream
		}
		u = u<<8 | uint64(b.stream[b.pos])
		b.pos++
	}
	for ; nbits > 0; nbits-- {
		bit, err := b.readBit()
		if err != nil {
			return 0, err
		}
		u <<= 1
		if bit {
			u |= 1
		}
	}
	return u, nil
}

func (b *bstreamReader) readByte() (byte, error) {
	u, err := b.readBits(8)
	return byte(u), err
}

func (b *bstreamReader) readVarint() (int64, error) {
	var u uint64
	for shift := uint(0); ; shift += 7 {
		if shift >= 64 {
			return 0, errors.New("chunkenc: varint overflows 64 bits")
		}
		v, err := b.readByte()
		if err != nil {
			return 0, err
		}
		u |= uint64(v&0x7f) << shift
		if v < 0x80 {
			break
		}
	}
	// Undo zig-zag.
	i := int64(u >> 1)
	if u&1 != 0 {
		i = ^i
	}
	return i, nil
}

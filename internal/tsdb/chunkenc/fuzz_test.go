package chunkenc

import (
	"math"
	"testing"
)

// FuzzChunk decodes arbitrary bytes as a chunk.
//
// The decoder is a bit reader over a non-byte-aligned stream, driven by a
// sample count in the header and by bucket widths it reads out of the data
// itself — every one of those is a length the file gets to choose, which is
// the classic shape of a decoder that can be made to read past its buffer or
// allocate a gigabyte. A chunk comes off a disk that may have been corrupted
// or truncated, so "arbitrary bytes" is not a hypothetical input.
//
// The contract is only that it does not panic and does not hang: garbage in
// may perfectly well decode to garbage samples, because a chunk carries no
// checksum of its own. Its integrity is the block's `chunks.dat` crc32c, one
// level up.
func FuzzChunk(f *testing.F) {
	c := NewChunk()
	a, err := c.Appender()
	if err != nil {
		f.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if err := a.Append(int64(i)*10_000, float64(i)*1.5); err != nil {
			f.Fatal(err)
		}
	}
	f.Add(c.Bytes())
	f.Add([]byte{})
	f.Add([]byte{0, 0})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0, 1, 0x80})       // claims a sample, no room for it
	f.Add([]byte{0x7f, 0xff, 0x01}) // claims 32767 samples

	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := FromBytes(b)
		if err != nil {
			return
		}
		// NumSamples is attacker-controlled: it must never be used as an
		// allocation size anywhere, and the iterator must stop at the end of
		// the data rather than at the count the header claims.
		n := c.NumSamples()
		it := c.Iterator()
		read := 0
		for it.Next() {
			ts, v := it.At()
			_, _ = ts, math.Float64bits(v)
			read++
			if read > n {
				t.Fatalf("iterator produced %d samples, header claims %d", read, n)
			}
		}
		_ = it.Err()
		// Appending to a decoded chunk is the resume path, which re-reads the
		// whole stream to find the tail. A corrupt chunk must fail it, not
		// crash it.
		if a, err := c.Appender(); err == nil {
			_ = a.Append(math.MaxInt64, math.NaN())
		}
	})
}

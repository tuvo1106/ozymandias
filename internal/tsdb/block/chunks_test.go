package block

import (
	"encoding/binary"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// writeChunksFile lays down a chunks.dat with a valid header and body after
// it, for tests that need records the writer would never produce.
func writeChunksFile(t *testing.T, body []byte) *chunkReader {
	t.Helper()
	dir := t.TempDir()
	file := make([]byte, 0, chunksHeaderLen+len(body))
	file = binary.BigEndian.AppendUint32(file, chunksMagic)
	file = append(file, chunksVersion)
	file = append(file, body...)
	if err := os.WriteFile(filepath.Join(dir, ChunksFilename), file, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := openChunkReader(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.close() })
	return r
}

// TestChunkReader_ALengthPrefixCannotWrapPastTheRecord.
//
// The record's length prefix was validated as used+length+4 == recLen. That
// sum is uint64 arithmetic on a number the file supplies: a ten-byte uvarint
// can claim a length so large that the sum wraps all the way round onto
// exactly recLen, so the guard agreed. The slice taken on the next line wraps
// the same way, giving a lower bound above its upper bound — rec[10:6] — and
// the read panicked instead of reporting a corrupt block.
//
// recLen comes from index.dat, so this is reachable from any corrupt or
// crafted block on disk, and AGENTS.md §4 puts panics outside main and tests
// out of bounds.
func TestChunkReader_ALengthPrefixCannotWrapPastTheRecord(t *testing.T) {
	for recLen := uint64(10); recLen <= 13; recLen++ {
		// The length that makes used+length+4 wrap back onto recLen, given
		// the ten bytes the uvarint itself occupies.
		claimed := -(14 - recLen) // i.e. 2^64 - (14 - recLen), unsigned
		body := binary.AppendUvarint(nil, claimed)
		if len(body) != 10 {
			t.Fatalf("recLen %d: the crafted uvarint is %d bytes, want 10", recLen, len(body))
		}
		body = append(body, make([]byte, recLen-uint64(len(body)))...)

		r := writeChunksFile(t, body)
		_, _, err := r.at(chunksHeaderLen, recLen)
		if err == nil {
			t.Errorf("recLen %d: a record claiming %d payload bytes was accepted", recLen, claimed)
		}
	}
}

// TestChunkReader_AnOffsetCannotWrapIntoTheFile is the same arithmetic one
// line up: off+recLen was compared against the file size, and an offset near
// the top of the uint64 range wraps back inside it. The check passed, and
// int64(off) then went negative for ReadAt, which reported a confusing error
// about a negative offset rather than a corrupt block.
func TestChunkReader_AnOffsetCannotWrapIntoTheFile(t *testing.T) {
	r := writeChunksFile(t, make([]byte, 64))
	for _, off := range []uint64{math.MaxUint64 - 10, math.MaxUint64, 1 << 63} {
		if _, _, err := r.at(off, 16); err == nil {
			t.Errorf("offset %d was accepted against a %d-byte file", off, r.size)
		}
	}
}

// FuzzChunkReader. chunks.dat was the only on-disk decoder in the TSDB with no
// fuzz target — chunkenc, wal and index.dat all have one — which is why the
// overflow above survived three review rounds.
//
// The record is fed as an arbitrary body and read with the length the index
// would have recorded for it, plus a few lengths that disagree with it: the
// index and the file are written separately, so a bug in the writer, or a
// block from a future version, is exactly the case where they do not match.
// Any outcome is fine except a panic.
func FuzzChunkReader(f *testing.F) {
	f.Add([]byte{}, uint64(0))
	f.Add([]byte{0x01, 0x07, 0x00, 0x00, 0x00, 0x00}, uint64(6))
	f.Add(binary.AppendUvarint(nil, math.MaxUint64), uint64(10))
	f.Add(binary.AppendUvarint(nil, 1<<40), uint64(11))

	f.Fuzz(func(t *testing.T, body []byte, recLen uint64) {
		r := writeChunksFile(t, body)
		for _, l := range []uint64{recLen, uint64(len(body)), uint64(len(body)) + 4} {
			_, data, err := r.at(chunksHeaderLen, l)
			if err != nil {
				continue
			}
			// It parsed, which means the crc agreed with the body — the
			// writer's own shape. Then the record must account for itself
			// exactly: the uvarint, one encoding byte, the payload and the
			// crc, and nothing left over. An accepted record that does not
			// add up is how the overflow above presented.
			_, used := binary.Uvarint(body)
			if got := uint64(used) + 1 + uint64(len(data)) + 4; got != l {
				t.Fatalf("accepted a %d-byte record that accounts for %d bytes "+
					"(uvarint %d, payload %d)", l, got, used, len(data))
			}
		}
	})
}

// TestChunkReader_AWellFormedRecordStillReads guards the fix from the other
// side: the tightened bounds must not reject anything the writer produces.
func TestChunkReader_AWellFormedRecordStillReads(t *testing.T) {
	payload := []byte{7, 1, 2, 3, 4}
	body := binary.AppendUvarint(nil, uint64(len(payload)))
	body = append(body, payload...)
	body = binary.BigEndian.AppendUint32(body, crc32.Checksum(payload, castagnoli))

	r := writeChunksFile(t, body)
	enc, data, err := r.at(chunksHeaderLen, uint64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	if enc != 7 || string(data) != string(payload[1:]) {
		t.Errorf("read encoding %d data %v, want 7 and %v", enc, data, payload[1:])
	}
}

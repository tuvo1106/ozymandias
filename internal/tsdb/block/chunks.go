package block

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

// ChunksFilename holds the block's compressed sample data.
const ChunksFilename = "chunks.dat"

// chunks.dat layout:
//
//	magic u32 | version u8
//	repeated: uvarint len | encoding u8 | data[len] | crc32c u32
//
// The crc covers the encoding byte and the data. A chunk reference in the
// index is the byte offset of the record's length field, so reading one chunk
// is a single pread at a known offset — no scan, no index of its own.
const (
	chunksMagic   uint32 = 0x4f5a4348 // "OZCH"
	chunksVersion uint8  = 1
	// chunksHeaderLen is where the first record starts.
	chunksHeaderLen = 5
)

// EncodingGorilla marks a chunk written by [chunkenc]. The byte exists so a
// later encoding (M2 PR 2's sketches, or a constant-value chunk) can live in
// the same file, and so a reader that meets one it does not know can say so
// instead of decoding garbage.
const EncodingGorilla uint8 = 1

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// chunkWriter appends chunk records and remembers where each one landed.
type chunkWriter struct {
	f   *os.File
	buf []byte
	off uint64
}

func newChunkWriter(dir string) (*chunkWriter, error) {
	f, err := os.OpenFile(filepath.Join(dir, ChunksFilename), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, 0, chunksHeaderLen)
	hdr = binary.BigEndian.AppendUint32(hdr, chunksMagic)
	hdr = append(hdr, chunksVersion)
	if _, err := f.Write(hdr); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("block: writing chunks header: %w", err)
	}
	return &chunkWriter{f: f, off: chunksHeaderLen}, nil
}

// write appends one chunk and returns the offset and total record length to
// reference it by.
func (w *chunkWriter) write(encoding uint8, data []byte) (off, length uint64, err error) {
	at := w.off
	w.buf = w.buf[:0]
	w.buf = binary.AppendUvarint(w.buf, uint64(len(data))+1) // +1 for the encoding byte
	w.buf = append(w.buf, encoding)
	w.buf = append(w.buf, data...)
	crc := crc32.Checksum(w.buf[len(w.buf)-len(data)-1:], castagnoli)
	w.buf = binary.BigEndian.AppendUint32(w.buf, crc)

	if _, err := w.f.Write(w.buf); err != nil {
		return 0, 0, fmt.Errorf("block: writing chunk: %w", err)
	}
	w.off += uint64(len(w.buf))
	return at, uint64(len(w.buf)), nil
}

func (w *chunkWriter) close() error {
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		return fmt.Errorf("block: syncing chunks: %w", err)
	}
	return w.f.Close()
}

// chunkReader reads chunk records at the offsets the index gives it.
//
// It holds an open file rather than the bytes: a block's chunks are the bulk
// of its size, and a query usually wants a handful of series out of thousands.
// Production systems mmap the file and let the page cache decide what stays
// resident; pread is the same idea without the tricky parts, and the
// difference is measurable but not structural.
type chunkReader struct {
	f    *os.File
	size int64
}

func openChunkReader(dir string) (*chunkReader, error) {
	f, err := os.Open(filepath.Join(dir, ChunksFilename))
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	hdr := make([]byte, chunksHeaderLen)
	if _, err := io.ReadFull(f, hdr); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("block: reading chunks header: %w", err)
	}
	if got := binary.BigEndian.Uint32(hdr); got != chunksMagic {
		_ = f.Close()
		return nil, fmt.Errorf("block: %s is not a chunks file (magic %#x)", dir, got)
	}
	if hdr[4] != chunksVersion {
		_ = f.Close()
		return nil, fmt.Errorf("block: chunks version %d, this build reads %d", hdr[4], chunksVersion)
	}
	return &chunkReader{f: f, size: info.Size()}, nil
}

// maxChunkLen bounds what a length prefix may claim, so a corrupt one cannot
// make us allocate gigabytes before the crc gets a chance to reject it.
const maxChunkLen = 1 << 20

// at reads the chunk record of recLen bytes starting at off, verifying its
// checksum. The index stores both numbers so this is a single pread: the
// length prefix in the file is what makes the file walkable on its own (for
// repair and for `tsdb inspect`), not what a normal read depends on.
func (r *chunkReader) at(off, recLen uint64) (uint8, []byte, error) {
	if recLen < 6 || recLen > maxChunkLen { // uvarint + encoding + crc at minimum
		return 0, nil, fmt.Errorf("block: chunk at %d claims a record of %d bytes", off, recLen)
	}
	if off+recLen > uint64(r.size) {
		return 0, nil, fmt.Errorf("block: chunk at %d..%d is past the end (%d)", off, off+recLen, r.size)
	}
	rec := make([]byte, recLen)
	if _, err := r.f.ReadAt(rec, int64(off)); err != nil {
		return 0, nil, fmt.Errorf("block: reading chunk at %d: %w", off, err)
	}
	length, used := binary.Uvarint(rec)
	if used <= 0 || length < 1 || uint64(used)+length+4 != recLen {
		return 0, nil, fmt.Errorf("block: chunk at %d has a corrupt length prefix", off)
	}
	body := rec[used : uint64(used)+length]
	want := binary.BigEndian.Uint32(rec[uint64(used)+length:])
	if got := crc32.Checksum(body, castagnoli); got != want {
		return 0, nil, fmt.Errorf("block: chunk at %d is corrupt (crc %#x, want %#x)", off, got, want)
	}
	return body[0], body[1:], nil
}

func (r *chunkReader) close() error { return r.f.Close() }

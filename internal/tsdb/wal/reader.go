package wal

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

// Reader replays a log: every checkpoint in order, then every segment in
// order. Use it before writing, to rebuild state and to learn where the good
// data ends.
//
//	r, err := wal.NewReader(dir)
//	for r.Next() {
//	    rec := r.Record()
//	}
//	if err := r.Err(); err != nil { … }
//	end := r.End() // pass to Truncate to drop a torn tail
type Reader struct {
	dir   string
	files []string

	idx  int
	f    *os.File
	br   *bufio.Reader
	off  int64
	last bool // reading the final file, where a partial record is expected
	// strict disables the torn-tail allowance. Checkpointing sets it: when a
	// single segment is replayed in order to copy records forward, it is the
	// "last" file by construction, and treating damage in it as an expected
	// crash artefact would quietly launder corruption into the checkpoint —
	// where nothing would ever question it again.
	strict bool

	rec Record
	buf []byte
	err error
	end Position
}

// Position is where the last intact record ended.
type Position struct {
	// File is the segment or checkpoint file, relative to the log directory.
	File string
	// Offset is the byte offset just past the last good record.
	Offset int64
	// Segment is the segment number, or -1 for a checkpoint.
	Segment int
}

// NewReader opens the log in dir for replay. An empty or missing directory
// replays as an empty log, which is what a first start looks like.
func NewReader(dir string) (*Reader, error) {
	checkpoints, err := listNumbered(dir, checkpointPre, "")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Reader{dir: dir}, nil
		}
		return nil, err
	}
	segments, err := listNumbered(dir, "", segmentExt)
	if err != nil {
		return nil, err
	}
	r := &Reader{dir: dir, end: Position{Segment: -1}}
	for _, n := range checkpoints {
		r.files = append(r.files, fmt.Sprintf("%s%08d", checkpointPre, n))
	}
	for _, n := range segments {
		r.files = append(r.files, fmt.Sprintf("%08d%s", n, segmentExt))
	}
	return r, nil
}

// Record returns the record read by the last successful Next.
func (r *Reader) Record() Record { return r.rec }

// Err returns the first error that stopped replay. A torn tail in the final
// file is not an error; damage anywhere else is, and wraps ErrCorrupt.
func (r *Reader) Err() error { return r.err }

// End reports where the last intact record ended — the point a caller should
// truncate to before appending again.
func (r *Reader) End() Position { return r.end }

// Next advances to the next record.
func (r *Reader) Next() bool {
	for {
		if r.f == nil {
			if r.idx >= len(r.files) {
				return false
			}
			if !r.openNext() {
				return false
			}
		}
		ok, done := r.readRecord()
		if ok {
			return true
		}
		if done {
			return false
		}
		// End of this file: move to the next one.
		_ = r.f.Close()
		r.f, r.br = nil, nil
	}
}

func (r *Reader) openNext() bool {
	name := r.files[r.idx]
	f, err := os.Open(filepath.Join(r.dir, name))
	if err != nil {
		r.err = fmt.Errorf("wal: opening %s: %w", name, err)
		return false
	}
	r.f, r.br = f, bufio.NewReader(f)
	r.off = 0
	r.last = r.idx == len(r.files)-1
	r.idx++
	r.end = Position{File: name, Offset: 0, Segment: segmentNumber(name)}
	return true
}

// readRecord returns (got a record, replay is over).
func (r *Reader) readRecord() (bool, bool) {
	var hdr [headerSize]byte
	n, err := io.ReadFull(r.br, hdr[:])
	switch {
	case errors.Is(err, io.EOF) && n == 0:
		return false, false // clean end of file
	case err != nil:
		// A short header at the very end of the last file is a torn write.
		return false, r.torn(fmt.Errorf("short header (%d bytes)", n))
	}
	length := binary.BigEndian.Uint32(hdr[0:4])
	typ := hdr[4]
	want := binary.BigEndian.Uint32(hdr[5:9])

	if typ == 0 || length > MaxRecordSize {
		// Zero type is reserved, so a run of zeros (a pre-allocated or
		// partially written tail) lands here rather than being replayed.
		return false, r.torn(fmt.Errorf("implausible header: type %d, length %d", typ, length))
	}
	if cap(r.buf) < int(length) {
		r.buf = make([]byte, length)
	}
	payload := r.buf[:length]
	if n, err := io.ReadFull(r.br, payload); err != nil {
		return false, r.torn(fmt.Errorf("short payload (%d of %d bytes)", n, length))
	}
	if got := crc32.Checksum(payload, castagnoli); got != want {
		return false, r.torn(fmt.Errorf("crc mismatch: got %08x, want %08x", got, want))
	}
	r.off += int64(headerSize) + int64(length)
	r.end.Offset = r.off
	r.rec = Record{Type: typ, Data: payload}
	return true, false
}

// torn decides what damage means. In the last file it is the expected result
// of a crash mid-write: stop quietly. Anywhere earlier, the log is missing
// data it already acknowledged, and that must be reported rather than skipped.
func (r *Reader) torn(cause error) bool {
	if r.last && !r.strict {
		return true // stop replay, no error
	}
	r.err = fmt.Errorf("%w in %s at offset %d: %w", ErrCorrupt, r.end.File, r.off, cause)
	return true
}

// Close releases the open file, if any.
func (r *Reader) Close() error {
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

func segmentNumber(name string) int {
	if filepath.Ext(name) != segmentExt {
		return -1
	}
	var n int
	if _, err := fmt.Sscanf(name, "%08d", &n); err != nil {
		return -1
	}
	return n
}

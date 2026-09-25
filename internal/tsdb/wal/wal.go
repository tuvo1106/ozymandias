package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Record framing (docs/formats/wal.md):
//
//	len  uint32   payload length
//	type uint8    caller-defined record type, 0 is reserved
//	crc  uint32   crc32c of the payload
//	payload
//
// The CRC covers the payload only; a corrupt length is caught by the read
// failing or by the CRC of whatever it lands on.
const (
	headerSize = 4 + 1 + 4
	// MaxRecordSize bounds one record, so a corrupt length cannot make the
	// reader allocate an arbitrary amount of memory. Callers batch below this.
	MaxRecordSize = 16 << 20
	// DefaultSegmentSize is where a segment is rolled.
	DefaultSegmentSize = 32 << 20

	segmentExt    = ".wal"
	checkpointPre = "checkpoint."
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt is returned when damage is found somewhere the log cannot
// explain as a torn tail. It names the file and offset.
var ErrCorrupt = errors.New("wal: corrupt record")

// Record is one entry. Type is the caller's; 0 is reserved as "unset" so a
// zero-filled region cannot masquerade as a valid record.
type Record struct {
	Type uint8
	Data []byte
}

// Options configure a log. The zero value is usable except for Dir.
type Options struct {
	// Dir holds the segments. Created if missing.
	Dir string
	// SegmentSize is the size at which a new segment starts.
	// Defaults to DefaultSegmentSize.
	SegmentSize int64
}

// WAL is an append-only log. It is safe for concurrent use; writes are
// serialized, because ordering is the point.
type WAL struct {
	dir         string
	segmentSize int64

	mu      sync.Mutex
	f       *os.File
	segment int
	size    int64
	closed  bool
	// broken is set when a write failed and the half-written record could not
	// be cut back off. Everything after it in the file is unreachable, so the
	// log refuses further writes rather than accepting data it knows will be
	// lost. See [WAL.Log].
	broken error
	hdr    [headerSize]byte // scratch, reused per record under mu
}

// Open opens or creates the log in opts.Dir, appending to the newest segment.
//
// It does not repair: a caller that wants the torn tail removed replays with
// Reader first, which reports where the good data ends, and then calls
// Truncate. Opening is therefore always safe.
func Open(opts Options) (*WAL, error) {
	if opts.Dir == "" {
		return nil, errors.New("wal: Dir is required")
	}
	if opts.SegmentSize <= 0 {
		opts.SegmentSize = DefaultSegmentSize
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("wal: creating %s: %w", opts.Dir, err)
	}
	w := &WAL{dir: opts.Dir, segmentSize: opts.SegmentSize}

	segs, err := w.segments()
	if err != nil {
		return nil, err
	}
	seg := 0
	if len(segs) > 0 {
		seg = segs[len(segs)-1]
	}
	if err := w.openSegment(seg); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *WAL) openSegment(n int) error {
	path := w.segmentPath(n)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("wal: opening %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("wal: stat %s: %w", path, err)
	}
	// A new file needs its directory entry synced, or a crash can lose the
	// file itself while its contents are safely on disk.
	if info.Size() == 0 {
		if err := syncDir(w.dir); err != nil {
			_ = f.Close()
			return err
		}
	}
	w.f, w.segment, w.size = f, n, info.Size()
	return nil
}

func (w *WAL) segmentPath(n int) string {
	return filepath.Join(w.dir, fmt.Sprintf("%08d%s", n, segmentExt))
}

// Log appends records. It buffers in the OS page cache; durability requires
// Sync. Records in one call land in one segment, so a reader never sees half
// a batch split across a rollover boundary.
func (w *WAL) Log(recs ...Record) error {
	if len(recs) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("wal: log is closed")
	}
	if w.broken != nil {
		return w.broken
	}

	var total int64
	for _, r := range recs {
		if r.Type == 0 {
			return errors.New("wal: record type 0 is reserved")
		}
		if len(r.Data) > MaxRecordSize {
			return fmt.Errorf("wal: record of %d bytes exceeds the %d limit", len(r.Data), MaxRecordSize)
		}
		total += int64(headerSize + len(r.Data))
	}
	// Roll before writing, never in the middle of a batch.
	if w.size > 0 && w.size+total > w.segmentSize {
		if err := w.roll(); err != nil {
			return err
		}
	}
	for _, r := range recs {
		binary.BigEndian.PutUint32(w.hdr[0:4], uint32(len(r.Data)))
		w.hdr[4] = r.Type
		binary.BigEndian.PutUint32(w.hdr[5:9], crc32.Checksum(r.Data, castagnoli))
		if _, err := w.f.Write(w.hdr[:]); err != nil {
			return w.undoPartial(fmt.Errorf("wal: writing header: %w", err))
		}
		if _, err := w.f.Write(r.Data); err != nil {
			return w.undoPartial(fmt.Errorf("wal: writing payload: %w", err))
		}
		w.size += int64(headerSize + len(r.Data))
	}
	return nil
}

// undoPartial cuts a half-written record back off the end of the segment and
// returns cause. The caller holds mu.
//
// A write can fail between the header and the payload — ENOSPC is the
// realistic way — and what it leaves behind is a partial record. The batch
// itself is fine: Log returns the error, so nothing was acknowledged. The
// danger is the *next* batch, which would be written behind bytes no reader
// can get past, so it would be acknowledged, served from memory, and gone at
// the next restart. w.size is the offset of the last complete record, which is
// exactly where the file should end.
//
// If the rollback itself fails, there is nothing further this can do about the
// file, so it stops the log instead: every later Log and Sync fails. A loud
// ozyd is recoverable — [Repair] cleans the tail at the next start — and
// a quiet one that keeps taking writes is not.
func (w *WAL) undoPartial(cause error) error {
	if err := w.f.Truncate(w.size); err != nil {
		w.broken = fmt.Errorf("wal: a partial record could not be removed, "+
			"so the log is no longer writable: %w", errors.Join(cause, err))
		return w.broken
	}
	if _, err := w.f.Seek(w.size, io.SeekStart); err != nil {
		w.broken = fmt.Errorf("wal: a partial record was removed but the log "+
			"could not be repositioned: %w", errors.Join(cause, err))
		return w.broken
	}
	return cause
}

func (w *WAL) roll() error {
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("wal: syncing before rollover: %w", err)
	}
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("wal: closing segment: %w", err)
	}
	return w.openSegment(w.segment + 1)
}

// Sync flushes the current segment to stable storage. Until it returns, a
// power cut can lose anything Log has written.
func (w *WAL) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("wal: log is closed")
	}
	if w.broken != nil {
		return w.broken
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("wal: sync: %w", err)
	}
	return nil
}

// Close syncs and closes the log.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	err := w.f.Sync()
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Segment returns the segment currently being written.
func (w *WAL) Segment() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.segment
}

// segments lists segment numbers in order.
func (w *WAL) segments() ([]int, error) {
	return listNumbered(w.dir, "", segmentExt)
}

func listNumbered(dir, prefix, ext string) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("wal: reading %s: %w", dir, err)
	}
	var out []int
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ext) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, prefix), ext))
		if err != nil {
			continue // not ours
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("wal: opening dir %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("wal: syncing dir %s: %w", dir, err)
	}
	return nil
}

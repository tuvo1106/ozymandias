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
	"sync/atomic"
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
	// RecordOverhead is the framing each record adds to its payload on disk,
	// for callers that budget bytes before writing (M7's queue cap).
	RecordOverhead = headerSize
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

// ErrSync wraps every fsync failure the log reports: Sync, the segment sync
// and directory sync a rollover does, and Close.
//
// What to do about one is the caller's policy, set by
// [Options].StopOnSyncError. After a failed fsync Linux may already have
// dropped the dirty pages, and a later fsync can report success without
// writing them (PostgreSQL's 2018 "fsyncgate"), so every log ozyd writes
// stops: the queue, the TSDB and the log store. A stopped store fails every
// write, and says so on /readyz, in a gauge and in the log, so that it does not
// do it while looking healthy.
var ErrSync = errors.New("wal: fsync failed")

// ErrBroken is wrapped by every error from a log that can no longer accept
// writes, whatever any caller's policy is. That is after a rollover that
// failed partway (the old segment closed, the next not open), or a partial
// record that could not be cut back off. The log stays stopped until the
// process restarts and [Repair] runs. A failed write such as ENOSPC is not
// this: the record is cut back off and the log stays usable.
var ErrBroken = errors.New("wal: log is no longer writable")

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
	// StopOnSyncError makes a failed fsync final: the log stops (ErrBroken)
	// and every later Log and Sync fails, until a restart.
	//
	// It is an option, not the default, because stopping is only right for
	// a caller that can say it has stopped (see [WAL.Broken]). Every log
	// ozyd writes turns it on: none may acknowledge after a failed fsync
	// (fsyncgate, see ErrSync). The decision is made here, under the log's
	// own lock, so that no Sync can slip in between a failed fsync and the
	// caller hearing about it and "succeed" over pages the kernel has
	// dropped.
	StopOnSyncError bool
	// SyncFile replaces the file fsync. It exists so that a package that owns a
	// log (the TSDB, the log store) can test what it does when an fsync fails,
	// which no test can otherwise make a disk do. Nil means (*os.File).Sync.
	SyncFile func(*os.File) error
}

// WAL is an append-only log. It is safe for concurrent use; writes are
// serialized, because ordering is the point.
type WAL struct {
	dir         string
	segmentSize int64
	stopOnSync  bool
	syncFile    func(*os.File) error

	mu      sync.Mutex
	f       *os.File
	segment int
	size    int64
	closed  bool
	// broken is set by breakLocked: after a rollover that failed partway,
	// or a half-written record that could not be cut back off. The log then
	// refuses further writes rather than accepting data it knows will be
	// lost. See [ErrBroken].
	broken error
	// brokenView is broken, published for Broken(): readiness and a gauge ask
	// whether the log stopped, and they must get an answer while mu is held across a
	// stalled fsync, which is exactly when it matters.
	brokenView atomic.Pointer[error]
	hdr        [headerSize]byte // scratch, reused per record under mu
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
	if opts.SyncFile == nil {
		opts.SyncFile = (*os.File).Sync
	}
	w := &WAL{dir: opts.Dir, segmentSize: opts.SegmentSize, stopOnSync: opts.StopOnSyncError, syncFile: opts.SyncFile}

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
			return fmt.Errorf("%w: %w", ErrSync, err)
		}
	}
	w.f, w.segment, w.size = f, n, info.Size()
	return nil
}

func (w *WAL) segmentPath(n int) string {
	return filepath.Join(w.dir, SegmentName(n))
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
		return w.breakLocked(fmt.Errorf("a partial record could not be removed: %w", errors.Join(cause, err)))
	}
	if _, err := w.f.Seek(w.size, io.SeekStart); err != nil {
		return w.breakLocked(fmt.Errorf("a partial record was removed but the log "+
			"could not be repositioned: %w", errors.Join(cause, err)))
	}
	return cause
}

// breakLocked stops the log for good and returns the error it now reports.
// The first cause wins. The caller holds mu.
func (w *WAL) breakLocked(cause error) error {
	if w.broken == nil {
		w.broken = fmt.Errorf("%w: %w", ErrBroken, cause)
		e := w.broken
		w.brokenView.Store(&e)
	}
	return w.broken
}

// roll closes the current segment and opens the next. A failed sync comes
// first and leaves the log as it was, reported as ErrSync for the caller's
// policy to judge. A failed close or open stops the log: w.f then points at
// a closed file, which no later write can use.
func (w *WAL) roll() error {
	if err := w.syncFile(w.f); err != nil {
		return w.syncFailedLocked(fmt.Errorf("%w before rollover: %w", ErrSync, err))
	}
	if err := w.f.Close(); err != nil {
		return w.breakLocked(fmt.Errorf("wal: closing segment: %w", err))
	}
	if err := w.openSegment(w.segment + 1); err != nil {
		return w.breakLocked(err)
	}
	return nil
}

// Broken returns why the log stopped, or nil while it is writable. It is how a
// store that owns a log reports that it has stopped (readiness, a gauge).
func (w *WAL) Broken() error {
	if p := w.brokenView.Load(); p != nil {
		return *p
	}
	return nil
}

// Mark is a file in the log's directory and how many of its bytes are good.
type Mark struct {
	Name string
	Size int64
}

// Cut makes everything logged so far durable and returns each file's size at
// that moment, under the log's lock, so no record is half in it. Copying each
// file's first Size bytes yields a log that replays to exactly the state at the
// cut, however much is appended while the copy runs: a log only grows at its
// tail, and truncation (which rewrites files) is the caller's to hold off.
// It is how a backup takes a consistent copy of a log that is being written.
func (w *WAL) Cut() ([]Mark, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, errors.New("wal: log is closed")
	}
	if w.broken != nil {
		return nil, w.broken
	}
	if err := w.syncFile(w.f); err != nil {
		return nil, w.syncFailedLocked(fmt.Errorf("%w: %w", ErrSync, err))
	}
	es, err := os.ReadDir(w.dir)
	if err != nil {
		return nil, fmt.Errorf("wal: %w", err)
	}
	var out []Mark
	for _, e := range es {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, fmt.Errorf("wal: %w", err)
		}
		out = append(out, Mark{Name: e.Name(), Size: info.Size()})
	}
	return out, nil
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
	if err := w.syncFile(w.f); err != nil {
		return w.syncFailedLocked(fmt.Errorf("%w: %w", ErrSync, err))
	}
	return nil
}

// syncFailedLocked applies the StopOnSyncError policy to a failed fsync and
// returns the error to report. The caller holds mu.
func (w *WAL) syncFailedLocked(err error) error {
	if w.stopOnSync {
		return w.breakLocked(err)
	}
	return err
}

// Close syncs and closes the log.
func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if w.broken != nil {
		// A stopped log may hold a file a failed rollover already closed;
		// syncing it would report a made-up fsync failure over the real cause.
		_ = w.f.Close()
		return w.broken
	}
	var err error
	if serr := w.syncFile(w.f); serr != nil {
		err = fmt.Errorf("%w: %w", ErrSync, serr)
	}
	if cerr := w.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// End returns the position just past the last record written, in the
// segment currently being written. A caller that serializes its own Log calls
// (M7's queue does) learns from it where each record ended, which is what a
// later [NewSegmentReader] bounds its reads by.
//
// The position is of bytes *written*, not synced: pair it with Sync before
// treating anything up to it as durable.
func (w *WAL) End() Position {
	w.mu.Lock()
	defer w.mu.Unlock()
	return Position{File: SegmentName(w.segment), Offset: w.size, Segment: w.segment}
}

// StopOnSyncError reports whether this log was opened with
// [Options].StopOnSyncError.
func (w *WAL) StopOnSyncError() bool { return w.stopOnSync }

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

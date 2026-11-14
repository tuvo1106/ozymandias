package spool

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

const (
	recordType uint8 = 1
	// headerLen is the series count that prefixes every payload.
	headerLen = 4
	headFile  = "head"
	// headEvery is how many deliveries pass between cursor writes.
	headEvery = 8

	// DefaultMaxBytes is the spec's 512 MiB.
	DefaultMaxBytes = 512 << 20
	maxSegmentSize  = 8 << 20
)

// Options configures a Spool.
type Options struct {
	// Dir holds the segments and the cursor. Created if missing.
	Dir string
	// MaxBytes caps the spool on disk; past it the oldest segments are
	// dropped. Zero is DefaultMaxBytes.
	MaxBytes int64
	// SegmentSize is where a segment is rolled; zero picks MaxBytes/8 up to
	// 8 MiB. It is also the granularity of dropping.
	SegmentSize int64
	Logger      *slog.Logger

	// syncFn replaces the segment fsync, for tests.
	syncFn func(*wal.WAL) error
	// logFn replaces the append, for tests.
	logFn func(*wal.WAL, wal.Record) error
}

// Item is one payload read back from the spool.
type Item struct {
	// Series is how many series (or sketches, or spans) the payload carries,
	// for counting what a drop loses.
	Series int
	// Body is the gzip'd request body, as it was submitted.
	Body []byte

	seg int   // segment number
	end int64 // offset just past this record
}

// Stats describes the spool.
type Stats struct {
	// Payloads and Series are what is waiting to be delivered.
	Payloads, Series int
	Bytes            int64
	Segments         int
}

type segment struct {
	n int
	// goodEnd is the offset just past the last intact record. Bytes after it
	// (damage found at Open or while reading) are never read.
	goodEnd int64
	size    int64 // file size, for the cap
	records int
	series  int
}

// Spool is a FIFO of payloads on disk. It is safe for concurrent use.
type Spool struct {
	opts Options
	log  *slog.Logger

	mu   sync.Mutex
	w    *wal.WAL
	segs []segment // oldest first; the last is the one being appended to
	// off is the delivered cursor in segs[0]: records before it are done.
	off              int64
	doneRec, doneSer int // delivered records and series within segs[0]
	bytes            int64
	items, series    int
	peeked           *Item
	closed           bool
	// broken is set by a failed fsync and is final until restart: after it the
	// kernel may have dropped the pages (fsyncgate), so nothing more is accepted.
	broken error
	// sinceHead counts deliveries since the cursor was last written.
	sinceHead int
}

// Open opens the spool in opts.Dir, rebuilding its index from the segments.
// A torn tail is cut; a record damaged in the middle of a segment ends that
// segment at the last good record (see the package comment).
func Open(opts Options) (*Spool, error) {
	if opts.Dir == "" {
		return nil, errors.New("spool: Dir is required")
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.SegmentSize <= 0 {
		opts.SegmentSize = min(opts.MaxBytes/8, maxSegmentSize)
	}
	if opts.MaxBytes < 4*opts.SegmentSize {
		return nil, fmt.Errorf("spool: MaxBytes (%d) must be at least four segments (%d), or dropping a segment would drop most of the spool", opts.MaxBytes, opts.SegmentSize)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Spool{opts: opts, log: opts.Logger}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	if err := s.scan(); err != nil {
		return nil, err
	}
	w, err := wal.Open(wal.Options{Dir: opts.Dir, SegmentSize: opts.SegmentSize, StopOnSyncError: true})
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	s.w = w
	// The WAL appends to the newest segment; make sure the index has one.
	if len(s.segs) == 0 || s.segs[len(s.segs)-1].n != w.Segment() {
		s.segs = append(s.segs, segment{n: w.Segment()})
	}
	return s, nil
}

// scan rebuilds the index and reads the cursor.
func (s *Spool) scan() error {
	nums, err := wal.Segments(s.opts.Dir)
	if err != nil {
		return fmt.Errorf("spool: %w", err)
	}
	headSeg, headOff := s.readHead()
	for i, n := range nums {
		sg := segment{n: n}
		if st, err := os.Stat(filepath.Join(s.opts.Dir, wal.SegmentName(n))); err == nil {
			sg.size = st.Size()
		}
		useHead := i == 0 && n == headSeg
		r := wal.NewSegmentReader(s.opts.Dir, n, 0)
		for r.Next() {
			ser, ok := decodeSeries(r.Record())
			if !ok {
				break
			}
			sg.records++
			sg.series += ser
			sg.goodEnd = r.End().Offset
			if useHead && sg.goodEnd <= headOff {
				s.off, s.doneRec, s.doneSer = sg.goodEnd, sg.records, sg.series
			}
		}
		if err := r.Err(); err != nil {
			s.log.Warn("spool: a segment is damaged; its later records are skipped", "segment", n, "err", err)
		}
		_ = r.Close()
		if i == len(nums)-1 && sg.goodEnd < sg.size {
			// The newest segment's torn or damaged tail would sit in front of
			// everything appended next. Cut it.
			if err := os.Truncate(filepath.Join(s.opts.Dir, wal.SegmentName(n)), sg.goodEnd); err != nil {
				return fmt.Errorf("spool: cutting a damaged tail: %w", err)
			}
			sg.size = sg.goodEnd
		}
		s.segs = append(s.segs, sg)
		s.bytes += sg.size
		s.items += sg.records
		s.series += sg.series
	}
	s.items -= s.doneRec
	s.series -= s.doneSer
	return nil
}

func decodeSeries(r wal.Record) (int, bool) {
	if r.Type != recordType || len(r.Data) < headerLen {
		return 0, false
	}
	return int(binary.BigEndian.Uint32(r.Data)), true
}

// Put appends a payload and fsyncs it. If the spool is then over its cap the
// oldest segments are dropped; dropped is how many series that lost.
func (s *Spool) Put(series int, body []byte) (dropped int, err error) {
	if int64(wal.RecordOverhead+headerLen+len(body)) > s.opts.SegmentSize {
		// A record bigger than a segment would sit alone in one, and the cap can
		// never evict the newest segment, so the spool could stay over its cap.
		return 0, fmt.Errorf("spool: a payload of %d bytes does not fit a %d-byte segment", len(body), s.opts.SegmentSize)
	}
	rec := make([]byte, headerLen+len(body))
	binary.BigEndian.PutUint32(rec, uint32(series))
	copy(rec[headerLen:], body)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errors.New("spool: closed")
	}
	if s.broken != nil {
		return 0, s.broken
	}
	logf := s.opts.logFn
	if logf == nil {
		logf = func(w *wal.WAL, r wal.Record) error { return w.Log(r) }
	}
	if err := logf(s.w, wal.Record{Type: recordType, Data: rec}); err != nil {
		if errors.Is(err, wal.ErrSync) || errors.Is(err, wal.ErrBroken) {
			// The log has stopped itself (a rollover's fsync failed, or a
			// partial write could not be cut back). Say so, so that Broken()
			// and spool_enabled stop reporting a healthy buffer.
			s.broken = fmt.Errorf("spool: the log has stopped: %w", err)
			return 0, s.broken
		}
		return 0, fmt.Errorf("spool: %w", err)
	}
	sync := s.opts.syncFn
	if sync == nil {
		sync = (*wal.WAL).Sync
	}
	if err := sync(s.w); err != nil {
		// The record is in the file but not in the index, and the WAL has
		// stopped (StopOnSyncError), so no later Put can make it reachable. A
		// restart will find it and deliver it; the caller was told it failed, so
		// that is a repeat, never a loss.
		s.broken = fmt.Errorf("spool: stopped after a failed fsync: %w", err)
		return 0, s.broken
	}
	end := s.w.End()
	if last := s.segs[len(s.segs)-1]; last.n != end.Segment {
		s.segs = append(s.segs, segment{n: end.Segment})
	}
	last := &s.segs[len(s.segs)-1]
	s.bytes += end.Offset - last.size
	last.size, last.goodEnd = end.Offset, end.Offset
	last.records++
	last.series += series
	s.items++
	s.series += series

	for s.bytes > s.opts.MaxBytes && len(s.segs) > 1 {
		dropped += s.dropOldestLocked()
	}
	return dropped, nil
}

// dropOldestLocked deletes segs[0], returning the undelivered series it held.
func (s *Spool) dropOldestLocked() int {
	sg := s.segs[0]
	lostRec, lostSer := sg.records-s.doneRec, sg.series-s.doneSer
	if err := os.Remove(filepath.Join(s.opts.Dir, wal.SegmentName(sg.n))); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Error("spool: removing a segment", "segment", sg.n, "err", err)
	}
	s.bytes -= sg.size
	s.items -= lostRec
	s.series -= lostSer
	s.segs = s.segs[1:]
	s.off, s.doneRec, s.doneSer = 0, 0, 0
	s.peeked = nil
	s.writeHead()
	return lostSer
}

// Peek returns the oldest undelivered payload without removing it, or false if
// there is none. Peeking again before Ack returns the same item. skipped is how
// many series were lost to damage found on the way.
//
// err is a read that failed for a reason other than damage (no file
// descriptors, an I/O error). Nothing was changed; the caller should wait and
// ask again, and must not treat the spool as empty.
func (s *Spool) Peek() (it Item, ok bool, skipped int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peeked != nil {
		return *s.peeked, true, 0, nil
	}
	for {
		sg := s.segs[0]
		if s.off >= sg.goodEnd {
			if len(s.segs) == 1 {
				return Item{}, false, skipped, nil // the active segment, fully delivered
			}
			skipped += s.dropOldestLocked() // a finished or damaged segment, nothing left in it
			continue
		}
		r := wal.NewSegmentReader(s.opts.Dir, sg.n, s.off)
		if !r.Next() {
			err := r.Err()
			_ = r.Close()
			if !errors.Is(err, wal.ErrCorrupt) {
				// A read that failed for another reason (no file descriptors, an
				// I/O error) says nothing about the record. Leave everything as
				// it is and try again on the next call.
				s.log.Warn("spool: cannot read the next payload; will retry", "segment", sg.n, "err", err)
				return Item{}, false, skipped, err
			}
			s.log.Error("spool: damaged record; skipping the rest of its segment", "segment", sg.n, "err", err)
			skipped += s.skipDamagedLocked()
			continue
		}
		rec := r.Record()
		ser, good := decodeSeries(rec)
		end := r.End().Offset
		body := append([]byte(nil), rec.Data[headerLen:]...)
		_ = r.Close()
		if !good {
			s.log.Error("spool: malformed record; skipping the rest of its segment", "segment", sg.n)
			skipped += s.skipDamagedLocked()
			continue
		}
		s.peeked = &Item{Series: ser, Body: body, seg: sg.n, end: end}
		return *s.peeked, true, skipped, nil
	}
}

// Holds reports whether it is still the payload Peek would return: false once
// the cap has dropped the segment it came from, after which delivering it would
// count it both as lost and as sent.
func (s *Spool) Holds(it Item) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peeked != nil && s.peeked.seg == it.seg && s.peeked.end == it.end
}

// skipDamagedLocked gives up the rest of segs[0] from the cursor on, and
// returns the undelivered series that held.
//
// An older segment is simply marked finished and the caller drops it. The
// active segment cannot be: later Puts would append behind the damage and raise
// its good end again, and Peek would hit the same record for ever. So it is cut
// at the cursor and the log reopened, which starts appending after the cut.
func (s *Spool) skipDamagedLocked() int {
	if len(s.segs) > 1 {
		s.segs[0].goodEnd = s.off // the caller's next pass drops it, and dropOldestLocked counts what it held
		return 0
	}
	sg := &s.segs[0]
	lostRec, lostSer := sg.records-s.doneRec, sg.series-s.doneSer
	_ = s.w.Close()
	path := filepath.Join(s.opts.Dir, wal.SegmentName(sg.n))
	if err := os.Truncate(path, s.off); err != nil {
		// The damage is still in the file, so anything appended behind it would
		// be lost at the next restart. Stop rather than pretend.
		s.broken = fmt.Errorf("spool: cutting a damaged segment: %w", err)
		s.log.Error("spool: cutting a damaged segment", "err", err)
	}
	w, err := wal.Open(wal.Options{Dir: s.opts.Dir, SegmentSize: s.opts.SegmentSize, StopOnSyncError: true})
	if err != nil {
		s.broken = fmt.Errorf("spool: reopening after damage: %w", err)
		s.log.Error("spool: reopening after damage", "err", err)
	} else {
		s.w = w
	}
	s.bytes -= sg.size - s.off
	sg.size, sg.goodEnd = s.off, s.off
	sg.records, sg.series = s.doneRec, s.doneSer
	s.items -= lostRec
	s.series -= lostSer
	return lostSer
}

// Ack records that the item Peek returned was delivered (or refused for good).
func (s *Spool) Ack(it Item) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.peeked == nil || s.peeked.seg != it.seg || s.peeked.end != it.end {
		return // dropped by the cap while it was in flight
	}
	s.peeked = nil
	s.off = it.end
	s.doneRec++
	s.doneSer += it.Series
	s.items--
	s.series -= it.Series
	if sg := s.segs[0]; s.off >= sg.goodEnd && len(s.segs) > 1 {
		_ = s.dropOldestLocked() // nothing undelivered left, so nothing lost
		return
	}
	// The cursor is a file write and a rename; the next restart would repeat at
	// most headEvery payloads without it, which the intake absorbs.
	if s.sinceHead++; s.sinceHead >= headEvery {
		s.writeHead()
	}
}

// Broken reports whether a failed fsync has stopped the spool. It still serves
// what it holds; it accepts nothing more until a restart.
func (s *Spool) Broken() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.broken != nil
}

// Break stops the spool as a failed fsync does. Exported for the forwarder's
// tests, which need a stopped spool without a failing disk.
func (s *Spool) Break(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broken = err
}

// Stats returns a snapshot.
func (s *Spool) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Payloads: s.items, Series: s.series, Bytes: s.bytes, Segments: len(s.segs)}
}

// Close closes the log. What is in it stays for the next Open.
func (s *Spool) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.writeHead() // so a clean stop repeats nothing
	return s.w.Close()
}

// writeHead stores the cursor: "segment offset crc", replaced atomically and
// not fsynced (see the package comment). Failing to write it only means more
// is re-sent after a restart, so it is logged, not returned.
func (s *Spool) writeHead() {
	s.sinceHead = 0
	body := fmt.Sprintf("%d %d", s.segs[0].n, s.off)
	line := fmt.Sprintf("%s %d\n", body, crc32.ChecksumIEEE([]byte(body)))
	tmp := filepath.Join(s.opts.Dir, headFile+".tmp")
	if err := os.WriteFile(tmp, []byte(line), 0o644); err != nil {
		s.log.Warn("spool: writing the cursor", "err", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(s.opts.Dir, headFile)); err != nil {
		s.log.Warn("spool: replacing the cursor", "err", err)
	}
}

// readHead returns the stored cursor, or (-1, 0) if it is absent or does not
// check out. A bad cursor means starting the oldest segment again: duplicates,
// never loss.
func (s *Spool) readHead() (seg int, off int64) {
	b, err := os.ReadFile(filepath.Join(s.opts.Dir, headFile))
	if err != nil {
		return -1, 0
	}
	f := strings.Fields(string(b))
	if len(f) != 3 {
		return -1, 0
	}
	sum, err1 := strconv.ParseUint(f[2], 10, 32)
	n, err2 := strconv.Atoi(f[0])
	o, err3 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || o < 0 || uint32(sum) != crc32.ChecksumIEEE([]byte(f[0]+" "+f[1])) {
		return -1, 0
	}
	return n, o
}

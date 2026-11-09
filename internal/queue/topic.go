package queue

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// Defaults, from the M7 spec. The sync pair is measured in docs/notes/M7.md.
const (
	DefaultSegmentSize  = 32 << 20
	DefaultMaxBytes     = 2 << 30
	DefaultSyncInterval = 5 * time.Millisecond
	DefaultSyncBytes    = 1 << 20
)

var (
	// ErrFull is returned by Produce when the record would take the topic
	// past MaxBytes. It is the caller's signal to push back on its senders;
	// the queue never makes room by dropping records it has acknowledged.
	ErrFull = errors.New("queue: topic is full")
	// ErrClosed is returned once the topic has been closed.
	ErrClosed = errors.New("queue: topic is closed")
)

var consumerName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// Options configure a topic. Dir is required; zero values elsewhere take the
// defaults above.
type Options struct {
	// Dir is the topic's directory. Created if missing.
	Dir string
	// Clock times group commit and stamps records. Defaults to the real clock.
	Clock clock.Clock
	// SegmentSize is where a segment is rolled. Retention deletes whole
	// segments, so this is also the granularity at which space comes back.
	SegmentSize int64
	// MaxBytes caps the topic's size on disk; Produce returns ErrFull past it.
	MaxBytes int64
	// SyncInterval is how long the syncer waits after the first unsynced
	// append before fsyncing. A negative value syncs as soon as the syncer
	// wakes, with no batching window — the "fsync per request" baseline.
	SyncInterval time.Duration
	// SyncBytes ends the wait early once this many bytes are unsynced.
	SyncBytes int64

	// syncFn replaces the fsync, for fault-injection tests.
	syncFn func(*wal.WAL) error
	// afterUnlink runs in a retention pass between deleting the files and
	// updating the index, so a test can hold that window open.
	afterUnlink func()
}

// segInfo is what the topic knows about one segment without reading it.
type segInfo struct {
	n     int    // segment number
	first uint64 // offset of its first record; for an empty segment, the next offset to be written
	size  int64  // bytes
}

// syncPoint is the durable prefix of the topic: offsets below next are
// fsynced, and the last of them ends at byte off of segment seg.
type syncPoint struct {
	next uint64
	seg  int
	off  int64
}

// generation is one group commit: everyone who appended before the sync that
// releases it. err is written before done is closed.
type generation struct {
	done chan struct{}
	err  error
}

func newGeneration() *generation { return &generation{done: make(chan struct{})} }

// Topic is one durable log. It is safe for concurrent use: any number of
// producers, and any number of distinctly named consumers.
type Topic struct {
	dir          string
	clock        clock.Clock
	maxBytes     int64
	syncInterval time.Duration
	syncBytes    int64
	syncFn       func(*wal.WAL) error
	afterUnlink  func()
	w            *wal.WAL

	mu       sync.Mutex
	next     uint64    // next offset to assign
	segs     []segInfo // oldest first; the last is the one being written
	bytes    int64     // sum of segs[].size
	written  syncPoint // end of everything appended
	synced   syncPoint // end of everything durable
	notify   chan struct{}
	gen      *generation
	unsynced int64
	broken   error
	closed   bool
	// committed holds every consumer's committed next-offset: the ones
	// registered in this process and the ones found on disk at Open that have
	// not registered yet. Both hold back retention.
	committed map[string]uint64
	active    map[string]bool

	retMu sync.Mutex // serializes retention passes

	kick chan struct{}
	stop chan struct{}
	done chan struct{}
}

// Open opens or creates the topic in opts.Dir, recovering from a crash: the
// torn tail of the last segment is cut (it was never acknowledged), every
// record is read once to rebuild the segment index, and the offsets of every
// consumer that has ever committed are loaded.
//
// Reading the whole topic at startup costs time proportional to its size;
// that is the price of not keeping a separate index file that could itself
// disagree with the segments. A topic is normally near-empty (consumers keep
// up), so the cost is paid in full only after a long store outage.
func Open(opts Options) (*Topic, error) {
	if opts.Dir == "" {
		return nil, errors.New("queue: Dir is required")
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.SegmentSize <= 0 {
		opts.SegmentSize = DefaultSegmentSize
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.SyncInterval == 0 {
		opts.SyncInterval = DefaultSyncInterval
	}
	if opts.SyncBytes <= 0 {
		opts.SyncBytes = DefaultSyncBytes
	}
	if opts.syncFn == nil {
		opts.syncFn = (*wal.WAL).Sync
	}
	if err := os.MkdirAll(filepath.Join(opts.Dir, consumersDir), 0o755); err != nil {
		return nil, fmt.Errorf("queue: creating %s: %w", opts.Dir, err)
	}
	if _, err := wal.Repair(opts.Dir); err != nil {
		return nil, fmt.Errorf("queue: recovering %s: %w", opts.Dir, err)
	}
	segs, last, err := scan(opts.Dir)
	if err != nil {
		return nil, err
	}
	committed, err := loadOffsets(opts.Dir)
	if err != nil {
		return nil, err
	}

	// The next offset is one past the last record — or past the highest
	// committed offset, if that is further. It can be: retention may have
	// deleted every record, and a restart that then numbered from 0 again
	// would hand new records offsets a consumer has already committed past,
	// and the consumer would skip them.
	next := last
	for _, c := range committed {
		next = max(next, c)
	}
	for i := range segs {
		if segs[i].size == 0 {
			segs[i].first = next // only the newest can be empty
		}
	}
	oldest := next
	if len(segs) > 0 {
		oldest = segs[0].first
	}
	for name, c := range committed {
		if c < oldest {
			return nil, fmt.Errorf("%w: consumer %q resumes at offset %d, but the oldest record is %d",
				ErrCorrupt, name, c, oldest)
		}
	}

	w, err := wal.Open(wal.Options{Dir: opts.Dir, SegmentSize: opts.SegmentSize})
	if err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	end := w.End()
	if len(segs) == 0 || segs[len(segs)-1].n != end.Segment {
		segs = append(segs, segInfo{n: end.Segment, first: next, size: end.Offset})
	}
	t := &Topic{
		dir:          opts.Dir,
		clock:        opts.Clock,
		maxBytes:     opts.MaxBytes,
		syncInterval: opts.SyncInterval,
		syncBytes:    opts.SyncBytes,
		syncFn:       opts.syncFn,
		afterUnlink:  opts.afterUnlink,
		w:            w,
		next:         next,
		segs:         segs,
		notify:       make(chan struct{}),
		gen:          newGeneration(),
		committed:    committed,
		active:       map[string]bool{},
		kick:         make(chan struct{}, 1),
		stop:         make(chan struct{}),
		done:         make(chan struct{}),
	}
	for _, s := range segs {
		t.bytes += s.size
	}
	// Everything that survived Repair was synced by the run that wrote it
	// (an unsynced record that happens to survive a crash is durable now, and
	// its producer was simply never told).
	t.written = syncPoint{next: next, seg: end.Segment, off: end.Offset}
	t.synced = t.written
	go t.syncLoop()
	return t, nil
}

// scan reads every segment once, returning the index and one past the last
// offset found. Offsets must run consecutively across the whole topic; a gap
// or a step back is a topic this code did not write.
func scan(dir string) ([]segInfo, uint64, error) {
	nums, err := wal.Segments(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("queue: %w", err)
	}
	var (
		segs   []segInfo
		next   uint64
		seen   bool
		lastNo = -1
	)
	for _, n := range nums {
		if lastNo >= 0 && n != lastNo+1 {
			return nil, 0, fmt.Errorf("%w: segment %d follows segment %d", ErrCorrupt, n, lastNo)
		}
		lastNo = n
		info, err := os.Stat(filepath.Join(dir, wal.SegmentName(n)))
		if err != nil {
			return nil, 0, fmt.Errorf("queue: %w", err)
		}
		s := segInfo{n: n, size: info.Size()}
		r := wal.NewSegmentReader(dir, n, 0)
		empty := true
		for r.Next() {
			rec, err := decodeRecord(r.Record())
			if err != nil {
				_ = r.Close()
				return nil, 0, fmt.Errorf("segment %d: %w", n, err)
			}
			if seen && rec.Offset != next {
				_ = r.Close()
				return nil, 0, fmt.Errorf("%w: segment %d holds offset %d where %d was expected",
					ErrCorrupt, n, rec.Offset, next)
			}
			if empty {
				s.first = rec.Offset
				empty = false
			}
			seen = true
			next = rec.Offset + 1
		}
		err = r.Err()
		_ = r.Close()
		if err != nil {
			return nil, 0, fmt.Errorf("queue: segment %d: %w", n, err)
		}
		if empty && s.size > 0 {
			return nil, 0, fmt.Errorf("%w: segment %d has %d bytes and no records", ErrCorrupt, n, s.size)
		}
		if empty && n != nums[len(nums)-1] {
			return nil, 0, fmt.Errorf("%w: segment %d is empty but is not the newest", ErrCorrupt, n)
		}
		segs = append(segs, s)
	}
	return segs, next, nil
}

const (
	consumersDir = "consumers"
	offsetExt    = ".offset"
)

func loadOffsets(dir string) (map[string]uint64, error) {
	cdir := filepath.Join(dir, consumersDir)
	entries, err := os.ReadDir(cdir)
	if err != nil {
		return nil, fmt.Errorf("queue: %w", err)
	}
	out := map[string]uint64{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			// A commit that died before its rename. The old file is still
			// the truth; this one is just litter.
			if err := os.Remove(filepath.Join(cdir, name)); err != nil {
				return nil, fmt.Errorf("queue: removing %s: %w", name, err)
			}
			continue
		}
		cname, ok := strings.CutSuffix(name, offsetExt)
		if !ok || !consumerName.MatchString(cname) {
			continue // not ours
		}
		b, err := os.ReadFile(filepath.Join(cdir, name))
		if err != nil {
			return nil, fmt.Errorf("queue: %w", err)
		}
		next, err := decodeOffset(b)
		if err != nil {
			return nil, fmt.Errorf("consumer %q: %w", cname, err)
		}
		out[cname] = next
	}
	return out, nil
}

// Produce appends payload and returns its offset once it is durable.
//
// If ctx ends first, Produce returns ctx.Err() — but the record is already
// written and will still be synced and delivered. A caller that retries
// after a cancelled Produce therefore produces a duplicate, which is the
// at-least-once contract the consumers are built for, not a bug to avoid.
func (t *Topic) Produce(ctx context.Context, payload []byte) (uint64, error) {
	if len(payload) > MaxPayload {
		return 0, fmt.Errorf("queue: payload of %d bytes exceeds the %d limit", len(payload), MaxPayload)
	}
	size := int64(wal.RecordOverhead + recordHeader + len(payload))

	t.mu.Lock()
	switch {
	case t.closed:
		t.mu.Unlock()
		return 0, ErrClosed
	case t.broken != nil:
		t.mu.Unlock()
		return 0, t.broken
	case t.bytes+size > t.maxBytes:
		t.mu.Unlock()
		return 0, ErrFull
	}
	off := t.next
	if err := t.w.Log(wal.Record{Type: recordType, Data: encodeRecord(off, t.clock.Now(), payload)}); err != nil {
		// Nothing was appended (the WAL cut any partial record back off),
		// so the offset is not used up. ENOSPC lands here.
		t.mu.Unlock()
		return 0, fmt.Errorf("queue: %w", err)
	}
	end := t.w.End()
	t.next++
	if last := &t.segs[len(t.segs)-1]; last.n != end.Segment {
		t.segs = append(t.segs, segInfo{n: end.Segment, first: off})
	}
	last := &t.segs[len(t.segs)-1]
	t.bytes += end.Offset - last.size
	last.size = end.Offset
	t.written = syncPoint{next: t.next, seg: end.Segment, off: end.Offset}
	g := t.gen
	t.unsynced += size
	wake := t.unsynced == size || t.unsynced >= t.syncBytes
	t.mu.Unlock()

	if wake {
		select {
		case t.kick <- struct{}{}:
		default: // a wake-up is already pending
		}
	}
	select {
	case <-g.done:
		if g.err != nil {
			return 0, g.err
		}
		return off, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// syncLoop is the group committer: the topic's one background goroutine,
// stopped by Close.
func (t *Topic) syncLoop() {
	defer close(t.done)
	for {
		select {
		case <-t.stop:
			t.syncOnce()
			return
		case <-t.kick:
		}
		if t.syncInterval > 0 && !t.overSyncBytes() {
			timer := t.clock.NewTimer(t.syncInterval)
		wait:
			for {
				select {
				case <-timer.C():
					break wait
				case <-t.kick:
					if t.overSyncBytes() {
						timer.Stop()
						break wait
					}
				case <-t.stop:
					timer.Stop()
					t.syncOnce()
					return
				}
			}
		}
		t.syncOnce()
	}
}

func (t *Topic) overSyncBytes() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.unsynced >= t.syncBytes
}

// syncOnce fsyncs everything appended so far and releases its waiters.
//
// The fsync runs outside mu, so producers keep appending — into the next
// generation — while it is in flight. That overlap is what makes group commit
// pay: the next batch is accumulating while this one is being made durable.
//
// A failed fsync stops the topic for good. After a failed fsync Linux may
// have already dropped the dirty pages, so a retry can report success for
// data that never reached the disk (the 2018 "fsyncgate" finding that made
// Postgres panic on fsync failure). The only honest recovery is a restart,
// which re-reads what is actually on disk.
func (t *Topic) syncOnce() {
	t.mu.Lock()
	if t.unsynced == 0 || t.broken != nil {
		t.mu.Unlock()
		return
	}
	g, target := t.gen, t.written
	t.gen = newGeneration()
	t.unsynced = 0
	t.mu.Unlock()

	err := t.syncFn(t.w)

	t.mu.Lock()
	if err != nil {
		t.broken = fmt.Errorf("queue: fsync failed; %s accepts no more records until ozyd restarts: %w", t.dir, err)
		err = t.broken
		// Anyone who appended during the failed sync is in the next
		// generation, which will now never be synced.
		t.gen.err = t.broken
		close(t.gen.done)
	} else {
		t.synced = target
		close(t.notify)
		t.notify = make(chan struct{})
	}
	t.mu.Unlock()
	g.err = err
	close(g.done)
}

// Consumer registers (or re-attaches) the consumer called name. A consumer
// that has committed before resumes from its committed offset; a new one
// starts at the oldest record still retained, and its offset file is written
// at once so that it holds back retention from then on, across restarts too.
//
// Consumers are expected to register at startup, before anyone commits: a
// consumer added later starts wherever retention has got to.
func (t *Topic) Consumer(name string) (*Consumer, error) {
	if !consumerName.MatchString(name) {
		return nil, fmt.Errorf("queue: consumer name %q must match %s", name, consumerName)
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, ErrClosed
	}
	if t.active[name] {
		t.mu.Unlock()
		return nil, fmt.Errorf("queue: consumer %q is already registered", name)
	}
	start, known := t.committed[name]
	if !known {
		start = t.segs[0].first
	}
	t.active[name] = true
	t.committed[name] = start
	t.mu.Unlock()
	seg := t.locate(start)

	c := &Consumer{t: t, name: name, path: filepath.Join(t.dir, consumersDir, name+offsetExt),
		cursor: start, seg: seg}
	if !known {
		if err := writeOffsetFile(c.path, start); err != nil {
			t.mu.Lock()
			delete(t.active, name)
			delete(t.committed, name)
			t.mu.Unlock()
			return nil, err
		}
	}
	return c, nil
}

// retain deletes every segment whose records all sit below every consumer's
// committed offset. With no consumers it deletes nothing: an unread record
// is never garbage.
func (t *Topic) retain() error {
	t.retMu.Lock()
	defer t.retMu.Unlock()

	t.mu.Lock()
	if len(t.committed) == 0 {
		t.mu.Unlock()
		return nil
	}
	low := ^uint64(0)
	for _, c := range t.committed {
		low = min(low, c)
	}
	// Segment i is done when the next segment starts at or below low: every
	// offset in i is then below low. The newest segment is never deleted.
	k := 0
	for k < len(t.segs)-1 && t.segs[k+1].first <= low {
		k++
	}
	if k == 0 {
		t.mu.Unlock()
		return nil
	}
	before := t.segs[k].n
	t.mu.Unlock()

	// Only retention removes from the front of segs, and retMu serializes
	// it, so segs[:k] are still the same segments after this.
	if err := wal.Truncate(t.dir, before, nil); err != nil {
		return fmt.Errorf("queue: retention: %w", err)
	}
	if t.afterUnlink != nil {
		t.afterUnlink()
	}
	t.mu.Lock()
	for _, s := range t.segs[:k] {
		t.bytes -= s.size
	}
	t.segs = append([]segInfo(nil), t.segs[k:]...)
	t.mu.Unlock()
	return nil
}

// Stats is a snapshot of a topic, for self-metrics and /readyz.
type Stats struct {
	// Next is the offset the next record will get.
	Next uint64
	// Durable is one past the last fsynced offset: what consumers can see.
	Durable uint64
	// Oldest is the offset of the oldest retained record.
	Oldest uint64
	// Bytes is the topic's size on disk, and MaxBytes its cap.
	Bytes, MaxBytes int64
	// Segments is the number of segment files.
	Segments int
	// Lag is, per consumer, how many durable records it has not committed.
	Lag map[string]uint64
	// Broken is the error that stopped the topic, if any.
	Broken error
}

// Stats returns a snapshot.
func (t *Topic) Stats() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := Stats{
		Next: t.next, Durable: t.synced.next, Oldest: t.segs[0].first,
		Bytes: t.bytes, MaxBytes: t.maxBytes, Segments: len(t.segs),
		Lag: map[string]uint64{}, Broken: t.broken,
	}
	for name, c := range t.committed {
		s.Lag[name] = t.synced.next - c
	}
	return s
}

// Close syncs anything pending, releases every waiting producer and poller,
// and closes the log. It is safe to call more than once.
func (t *Topic) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.notify)
	t.notify = make(chan struct{})
	t.mu.Unlock()

	close(t.stop)
	<-t.done
	return t.w.Close()
}

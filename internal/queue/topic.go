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
	// logFn replaces the append, and openSyncFn the fsync Open does on
	// recovered records, for fault-injection tests.
	logFn      func(*wal.WAL, wal.Record) error
	openSyncFn func(*wal.WAL) error
}

// segInfo is what the topic knows about one segment without reading it.
type segInfo struct {
	n     int    // segment number
	first uint64 // offset of its first record; for an empty segment, the next offset to be written
	size  int64  // bytes
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
	logFn        func(*wal.WAL, wal.Record) error
	w            *wal.WAL

	mu       sync.Mutex
	next     uint64    // next offset to assign: everything below it is written
	segs     []segInfo // oldest first; the last is the one being written
	bytes    int64     // sum of segs[].size
	synced   uint64    // everything below it is durable, and visible to consumers
	notify   chan struct{}
	gen      *generation
	unsynced int64
	broken   error
	closed   bool
	// retainErr is the last retention pass's failure, nil once one succeeds.
	retainErr error
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

// Open opens or creates the topic in opts.Dir, recovering from a crash: every
// record is read once to rebuild the segment index, the torn tail of the last
// segment is cut (it was never acknowledged), the offsets of every consumer
// that has ever committed are loaded, and the newest segment is fsynced
// before anything in it is offered to a consumer.
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
	if opts.openSyncFn == nil {
		opts.openSyncFn = (*wal.WAL).Sync
	}
	if opts.logFn == nil {
		opts.logFn = func(w *wal.WAL, r wal.Record) error { return w.Log(r) }
	}
	if err := os.MkdirAll(filepath.Join(opts.Dir, consumersDir), 0o755); err != nil {
		return nil, fmt.Errorf("queue: creating %s: %w", opts.Dir, err)
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
	// The previous run may have died with records written but not synced.
	// A process crash keeps them in the page cache, so they read back as if
	// durable — but a power cut now would still lose them, after a consumer
	// had applied and committed them. Sync before offering them to anyone.
	// Older segments were synced by the rollover that closed them.
	if err := opts.openSyncFn(w); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("queue: syncing recovered records: %w", err)
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
		logFn:        opts.logFn,
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
	// Everything recovered is durable now (an unsynced record that survived
	// the crash was just synced above; its producer was never told, and will
	// resend it, which is a duplicate the consumers tolerate).
	t.synced = next
	go t.syncLoop()
	return t, nil
}

// scan is recovery: it reads the topic once, cuts a torn tail off the newest
// segment, and returns the segment index and one past the last offset.
//
// It does Repair's job itself rather than calling it, because Repair is a
// full read of its own and a topic after a long store outage can be
// gigabytes: reading it twice at the moment ozyd is trying to come back is
// the wrong place to spend the time. Only the 16-byte record header is
// decoded; payloads are not copied.
//
// Offsets must run consecutively across the whole topic, and segment numbers
// too; a gap or a step back is a topic this code did not write.
func scan(dir string) ([]segInfo, uint64, error) {
	r, err := wal.NewReader(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("queue: %w", err)
	}
	firsts := map[int]uint64{}
	var (
		next uint64
		seen bool
	)
	for r.Next() {
		off, err := recordOffset(r.Record())
		seg := r.End().Segment
		if err == nil && seg < 0 {
			err = fmt.Errorf("%w: a WAL checkpoint in a queue topic", ErrCorrupt)
		}
		if err == nil && seen && off != next {
			err = fmt.Errorf("%w: segment %d holds offset %d where %d was expected", ErrCorrupt, seg, off, next)
		}
		if err != nil {
			_ = r.Close()
			return nil, 0, fmt.Errorf("segment %d: %w", seg, err)
		}
		if _, ok := firsts[seg]; !ok {
			firsts[seg] = off
		}
		seen, next = true, off+1
	}
	readErr, end := r.Err(), r.End()
	if err := r.Close(); err != nil {
		return nil, 0, fmt.Errorf("queue: %w", err)
	}
	if readErr != nil {
		return nil, 0, fmt.Errorf("queue: recovering %s: %w", dir, readErr)
	}
	// The reader stops quietly at a torn tail; whatever is past the last
	// good record was never acknowledged and must go before anything is
	// appended behind it (wal.Repair explains why).
	if end.File != "" {
		info, err := os.Stat(filepath.Join(dir, end.File))
		if err != nil {
			return nil, 0, fmt.Errorf("queue: %w", err)
		}
		if info.Size() != end.Offset {
			if err := wal.TruncateTail(dir, end); err != nil {
				return nil, 0, fmt.Errorf("queue: cutting the torn tail: %w", err)
			}
		}
	}

	nums, err := wal.Segments(dir)
	if err != nil {
		return nil, 0, fmt.Errorf("queue: %w", err)
	}
	segs := make([]segInfo, 0, len(nums))
	for i, n := range nums {
		if i > 0 && n != nums[i-1]+1 {
			return nil, 0, fmt.Errorf("%w: segment %d follows segment %d", ErrCorrupt, n, nums[i-1])
		}
		info, err := os.Stat(filepath.Join(dir, wal.SegmentName(n)))
		if err != nil {
			return nil, 0, fmt.Errorf("queue: %w", err)
		}
		first, has := firsts[n]
		switch {
		case !has && info.Size() > 0:
			return nil, 0, fmt.Errorf("%w: segment %d has %d bytes and no records", ErrCorrupt, n, info.Size())
		case !has && i != len(nums)-1:
			return nil, 0, fmt.Errorf("%w: segment %d is empty but is not the newest", ErrCorrupt, n)
		}
		segs = append(segs, segInfo{n: n, first: first, size: info.Size()})
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
	if err := t.logFn(t.w, wal.Record{Type: recordType, Data: encodeRecord(off, t.clock.Now(), payload)}); err != nil {
		// Nothing was appended (the WAL cut any partial record back off),
		// so the offset is not used up. ENOSPC lands here, and is worth
		// retrying. A failed fsync — the one a rollover does before closing
		// the old segment — is not: it stops the topic exactly as a failed
		// group commit does, and fails everyone waiting on one.
		if errors.Is(err, wal.ErrSync) {
			err = t.breakLocked(err)
		} else {
			err = fmt.Errorf("queue: %w", err)
		}
		t.mu.Unlock()
		return 0, err
	}
	end := t.w.End()
	t.next++
	if last := &t.segs[len(t.segs)-1]; last.n != end.Segment {
		t.segs = append(t.segs, segInfo{n: end.Segment, first: off})
	}
	last := &t.segs[len(t.segs)-1]
	t.bytes += end.Offset - last.size
	last.size = end.Offset
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
	g, target := t.gen, t.next
	t.gen = newGeneration()
	t.unsynced = 0
	t.mu.Unlock()

	err := t.syncFn(t.w)

	t.mu.Lock()
	switch {
	case err != nil:
		err = t.breakLocked(err)
	case t.broken != nil:
		// A rollover's fsync failed while this one was in flight. This
		// sync may have run after it, on the same file, and "succeeded"
		// over pages the kernel had already dropped: it proves nothing.
		err = t.broken
	default:
		t.synced = target
		close(t.notify)
		t.notify = make(chan struct{})
	}
	t.mu.Unlock()
	g.err = err
	close(g.done)
}

// breakLocked stops the topic after a failed fsync and fails everyone waiting
// on the current generation, who would otherwise wait for a sync that will
// never come. It returns the error the topic now reports. The caller holds mu.
func (t *Topic) breakLocked(cause error) error {
	if t.broken != nil {
		return t.broken
	}
	t.broken = fmt.Errorf("queue: fsync failed; %s accepts no more records until ozyd restarts: %w", t.dir, cause)
	t.gen.err = t.broken
	close(t.gen.done)
	return t.broken
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
	// Registration is serialized with retention. A pass that worked out
	// what to delete before this consumer existed would otherwise delete
	// the segment its start offset points into, and its offset file would
	// then name a record that is gone — which Open refuses for ever after.
	t.retMu.Lock()
	defer t.retMu.Unlock()
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
//
// A failure is recorded (Stats.RetentionErr) rather than returned, because
// its caller is Commit, whose own work — the offset file — is already
// durable by then: reporting the commit as failed would invite the caller
// to redo it. Whatever happened, the index is reconciled with the files
// actually left on disk, so a partial failure cannot leave it listing a
// segment that is gone (a parked consumer would then report it missing)
// or charging deleted bytes against the cap.
func (t *Topic) retain() {
	t.retMu.Lock()
	defer t.retMu.Unlock()

	t.mu.Lock()
	if len(t.committed) == 0 {
		t.mu.Unlock()
		return
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
		return
	}
	before := t.segs[k].n
	t.mu.Unlock()

	// Only retention removes from the front of segs, and retMu serializes
	// it, so segs[:k] are still the same segments after this.
	err := wal.Truncate(t.dir, before, nil)
	if t.afterUnlink != nil {
		t.afterUnlink()
	}
	present := map[int]bool{}
	nums, listErr := wal.Segments(t.dir)
	for _, n := range nums {
		present[n] = true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if listErr != nil {
		// Cannot tell what is gone; keep the index as it was.
		t.retainErr = fmt.Errorf("queue: retention: %w", errors.Join(err, listErr))
		return
	}
	drop := 0
	for drop < k && !present[t.segs[drop].n] {
		t.bytes -= t.segs[drop].size
		drop++
	}
	t.segs = append([]segInfo(nil), t.segs[drop:]...)
	if err != nil {
		t.retainErr = fmt.Errorf("queue: retention: %w", err)
	} else {
		t.retainErr = nil
	}
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
	// RetentionErr is the last retention pass's failure, nil once one
	// succeeds. Space is not coming back while it is set.
	RetentionErr error
}

// Stats returns a snapshot.
func (t *Topic) Stats() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := Stats{
		Next: t.next, Durable: t.synced, Oldest: t.segs[0].first,
		Bytes: t.bytes, MaxBytes: t.maxBytes, Segments: len(t.segs),
		Lag: map[string]uint64{}, Broken: t.broken, RetentionErr: t.retainErr,
	}
	for name, c := range t.committed {
		s.Lag[name] = t.synced - c
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

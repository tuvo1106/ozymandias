package logstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/query/logql"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Defaults for [Options].
const (
	// DefaultBlockBytes is the raw size at which a stream's head block is
	// sealed: big enough that zstd has something to compress and a block
	// holds a useful time slice, small enough that a read decodes little to
	// find a few lines.
	DefaultBlockBytes = 256 << 10
	// DefaultBlockAge seals a head that is not filling, so a quiet stream's
	// logs reach a chunk (and stop pinning WAL segments) in bounded time.
	DefaultBlockAge = 5 * time.Minute
	// DefaultRetention is how long a day of logs is kept.
	DefaultRetention = 7 * 24 * time.Hour
	// DefaultTick is how often the background loop looks for heads to seal
	// and chunks to expire.
	DefaultTick = 10 * time.Second

	walRecordLogs uint8 = 1
	// maxWALRecord keeps one WAL record well under wal.MaxRecordSize; a batch
	// that is bigger is split across records.
	maxWALRecord = 8 << 20

	dayLayout = "2006-01-02"
)

// Options configure a [Store].
type Options struct {
	// Dir holds everything the store writes: the stream catalog, the WAL and
	// one directory per day of chunks. Required.
	Dir string
	// Clock is the time source for block age and retention; default real.
	Clock clock.Clock
	// Retention is how long a day is kept, measured from the end of the day.
	// Zero means DefaultRetention; negative keeps everything.
	Retention time.Duration
	// BlockBytes and BlockAge are the seal thresholds; zero means the default.
	BlockBytes int
	BlockAge   time.Duration
	// Tick is the background loop's period; zero means DefaultTick.
	Tick time.Duration
	// WALSegmentSize is the WAL's segment size; zero means the WAL's default.
	// Smaller segments are truncated sooner, which tests use to see it happen.
	WALSegmentSize int64
	// NoSync skips every fsync (the WAL after each Append, each sealed block,
	// the catalog). A crash can then lose batches the store acknowledged: for
	// benchmarks and tests of everything but durability, never for production.
	NoSync bool
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// stream is one label set's unsealed logs. Everything here is guarded by the
// store's locks as described on [Store].
type stream struct {
	id     int64
	labels labelSet

	head      []rawEntry // arrival order; sealed when big or old
	headBytes int
	headSince time.Time // when the head became non-empty
	headSeg   int       // WAL segment holding the oldest unsealed entry
	open      *chunkState
}

type chunkKey struct {
	stream int64
	day    string // YYYY-MM-DD, UTC
}

// chunkState is one chunk file as the store knows it: its path, the blocks
// that are published (searchable), and the writer if it is being appended to.
type chunkState struct {
	key    chunkKey
	path   string
	blocks []BlockMeta
	w      *chunkWriter
}

// Store is a Loki-style log store: logs grouped into streams (a unique label
// set), buffered per stream in a head block, sealed into compressed chunk
// files, and searched by index-then-scan. See doc.go for the mental model.
//
// # Locking
//
// Two locks, with a deliberate split. wmu serializes the write side — Append,
// sealing, retention, Close — which is what lets a seal write a block to disk
// without anyone else mutating the head it is reading. smu protects what a
// search reads (the stream map, each head, each chunk's published blocks) and
// is held only to *publish* a change, never across disk I/O. A search takes a
// snapshot under smu.RLock and then reads blocks lock-free, because a
// published block is immutable. The one rule that makes this correct: a seal
// writes and fsyncs its block first, and only then, under smu.Lock, adds it
// to the chunk's published list and empties the head, in one step. A search
// therefore sees every log exactly once: in the head or in a block, never
// both and never neither.
type Store struct {
	opts   Options
	clk    clock.Clock
	log    *slog.Logger
	dir    string
	walDir string

	wal *wal.WAL
	cat *catalog

	wmu sync.Mutex
	smu sync.RWMutex

	seq     uint64 // last WAL sequence number handed out; wmu
	streams map[string]*stream
	byID    map[int64]*stream
	chunks  map[chunkKey]*chunkState
	closed  bool

	stop chan struct{}
	done chan struct{}

	// afterBlock, if set (tests only), runs in seal after a block is durable
	// and published but before the WAL is truncated: the window in which a
	// crash leaves an entry in both places.
	afterBlock func()
}

// ErrClosed is returned by operations on a closed store.
var ErrClosed = errors.New("logstore: closed")

// Open opens (creating if needed) the store in opts.Dir and recovers it: the
// chunk files are indexed from their footers (or by walking their blocks), the
// WAL's torn tail is cut off, and every logged entry that no sealed block
// already holds is replayed into its stream's head.
func Open(opts Options) (*Store, error) {
	if opts.Dir == "" {
		return nil, errors.New("logstore: Dir is required")
	}
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Retention == 0 {
		opts.Retention = DefaultRetention
	}
	if opts.BlockBytes <= 0 {
		opts.BlockBytes = DefaultBlockBytes
	}
	if opts.BlockAge <= 0 {
		opts.BlockAge = DefaultBlockAge
	}
	if opts.Tick <= 0 {
		opts.Tick = DefaultTick
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("logstore: %w", err)
	}
	s := &Store{
		opts: opts, clk: opts.Clock, log: opts.Logger.With("component", "logstore"),
		dir: opts.Dir, walDir: filepath.Join(opts.Dir, "wal"),
		streams: map[string]*stream{}, byID: map[int64]*stream{}, chunks: map[chunkKey]*chunkState{},
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	cat, err := openCatalog(filepath.Join(opts.Dir, "catalog.db"), opts.NoSync)
	if err != nil {
		return nil, err
	}
	s.cat = cat
	if err := s.recover(); err != nil {
		_ = cat.close()
		return nil, err
	}
	w, err := wal.Open(wal.Options{Dir: s.walDir, SegmentSize: opts.WALSegmentSize})
	if err != nil {
		_ = cat.close()
		return nil, fmt.Errorf("logstore: opening WAL: %w", err)
	}
	s.wal = w
	go s.loop()
	return s, nil
}

var dayDir = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
var chunkFile = regexp.MustCompile(`^(\d+)\.chunk$`)

// recover rebuilds the in-memory state: streams from the catalog, chunk
// indexes from the chunk files (which are the source of truth for what is
// stored), then the unsealed tail from the WAL.
func (s *Store) recover() error {
	rows, err := s.cat.streams()
	if err != nil {
		return fmt.Errorf("logstore: loading streams: %w", err)
	}
	for _, r := range rows {
		st := &stream{id: r.id, labels: r.labels}
		s.streams[r.labels.key()], s.byID[r.id] = st, st
	}

	// sealed[stream, day] is the highest sequence number any block of that
	// stream's chunk for that day holds. A WAL entry at or below it, for a
	// timestamp on that day, is already in a chunk and must not be replayed
	// into the head again. It is per day because a seal writes one block per
	// day (see seal): a crash between two of them leaves one day sealed and
	// the other not, and a single per-stream maximum would drop the second
	// day's entries.
	sealed := map[chunkKey]uint64{}
	days, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, d := range days {
		if !d.IsDir() || !dayDir.MatchString(d.Name()) {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.dir, d.Name()))
		if err != nil {
			return err
		}
		for _, f := range files {
			m := chunkFile.FindStringSubmatch(f.Name())
			if m == nil {
				continue
			}
			id, _ := strconv.ParseInt(m[1], 10, 64)
			path := filepath.Join(s.dir, d.Name(), f.Name())
			ix, err := indexFile(path)
			if err != nil {
				// A chunk that does not open is skipped, not fatal: one
				// damaged day must not take the whole store down. It is
				// left in place for inspection.
				s.log.Warn("skipping unreadable chunk", "path", path, "error", err)
				continue
			}
			if s.byID[id] == nil {
				s.log.Warn("chunk belongs to a stream the catalog does not know; skipping", "path", path)
				continue
			}
			cs := &chunkState{key: chunkKey{id, d.Name()}, path: path, blocks: ix.Blocks}
			s.chunks[cs.key] = cs
			for _, b := range ix.Blocks {
				sealed[cs.key] = max(sealed[cs.key], b.LastSeq)
				s.seq = max(s.seq, b.LastSeq)
			}
		}
	}

	if _, err := wal.Repair(s.walDir); err != nil {
		return fmt.Errorf("logstore: repairing WAL: %w", err)
	}
	r, err := wal.NewReader(s.walDir)
	if err != nil {
		return fmt.Errorf("logstore: reading WAL: %w", err)
	}
	defer func() { _ = r.Close() }()
	now := s.clk.Now()
	for r.Next() {
		rec := r.Record()
		if rec.Type != walRecordLogs {
			continue
		}
		base, bodies, err := decodeWALRecord(rec.Data)
		if err != nil {
			return fmt.Errorf("logstore: WAL record: %w", err)
		}
		seg := r.End().Segment
		for i, body := range bodies {
			seq := base + uint64(i)
			s.seq = max(s.seq, seq)
			l, err := decodeLog(body)
			if err != nil {
				return fmt.Errorf("logstore: WAL entry %d: %w", seq, err)
			}
			st, err := s.streamFor(l)
			if err != nil {
				return err
			}
			if seq <= sealed[chunkKey{st.id, dayOf(l.Ts)}] {
				continue
			}
			// The reader reuses its buffer for the next record, and the body
			// is a slice of it: keep a copy.
			st.add(rawEntry{Ts: l.Ts, Seq: seq, Body: bytes.Clone(body)}, now, seg)
		}
	}
	return r.Err()
}

func indexFile(path string) (ChunkIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return ChunkIndex{}, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return ChunkIndex{}, err
	}
	return readIndex(f, st.Size())
}

// labelsOf derives a log's stream labels. It is logql.LabelValue, the same
// function the scan filter uses, so the index and the scan cannot disagree.
func labelsOf(l *wire.Log) labelSet {
	var ls labelSet
	for i, k := range logql.StreamLabels {
		ls[i] = logql.LabelValue(l, k)
	}
	return ls
}

// streamFor finds or creates the stream for a log. wmu (or recovery, which is
// single-threaded) must be held: creating one writes to the catalog.
func (s *Store) streamFor(l *wire.Log) (*stream, error) {
	ls := labelsOf(l)
	key := ls.key()
	s.smu.RLock()
	st := s.streams[key]
	s.smu.RUnlock()
	if st != nil {
		return st, nil
	}
	id, err := s.cat.create(ls)
	if err != nil {
		return nil, err
	}
	st = &stream{id: id, labels: ls}
	s.smu.Lock()
	s.streams[key], s.byID[id] = st, st
	s.smu.Unlock()
	return st, nil
}

func (st *stream) add(e rawEntry, now time.Time, seg int) {
	if len(st.head) == 0 {
		st.headSince, st.headSeg = now, seg
	}
	st.head = append(st.head, e)
	st.headBytes += len(e.Body) + 16
}

func encodeLog(l *wire.Log) ([]byte, error) { return json.Marshal(l) }

// decodeLog reads a stored body back into a log, keeping numbers exact.
func decodeLog(body []byte) (*wire.Log, error) {
	var l wire.Log
	dec := json.NewDecoder(bytesReader(body))
	dec.UseNumber()
	if err := dec.Decode(&l); err != nil {
		return nil, err
	}
	return &l, nil
}

// WAL record: base sequence number (u64) | n (uvarint) | n × (len uvarint | body).
// Entry i has sequence number base+i.
func encodeWALRecord(base uint64, bodies [][]byte) []byte {
	n := 8 + binary.MaxVarintLen64
	for _, b := range bodies {
		n += binary.MaxVarintLen64 + len(b)
	}
	out := make([]byte, 8, n)
	binary.BigEndian.PutUint64(out, base)
	out = binary.AppendUvarint(out, uint64(len(bodies)))
	for _, b := range bodies {
		out = binary.AppendUvarint(out, uint64(len(b)))
		out = append(out, b...)
	}
	return out
}

func decodeWALRecord(p []byte) (uint64, [][]byte, error) {
	if len(p) < 9 {
		return 0, nil, errors.New("short record")
	}
	base := binary.BigEndian.Uint64(p)
	p = p[8:]
	n, k := binary.Uvarint(p)
	if k <= 0 || n > uint64(len(p)) {
		return 0, nil, errors.New("bad entry count")
	}
	p = p[k:]
	bodies := make([][]byte, 0, n)
	for i := uint64(0); i < n; i++ {
		l, k := binary.Uvarint(p)
		if k <= 0 || l > uint64(len(p)-k) {
			return 0, nil, errors.New("bad entry length")
		}
		bodies = append(bodies, p[k:k+int(l)])
		p = p[k+int(l):]
	}
	if len(p) != 0 {
		return 0, nil, errors.New("bytes after the last entry")
	}
	return base, bodies, nil
}

// Append stores a batch. When it returns nil every log is in the WAL and
// fsynced (unless NoSync): a crash after this point loses none of them. The
// logs must already be valid (wire.DecodeLogs); Append derives each one's
// stream from its labels.
func (s *Store) Append(ctx context.Context, batch []wire.Log) error {
	if len(batch) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	bodies := make([][]byte, len(batch))
	for i := range batch {
		b, err := encodeLog(&batch[i])
		if err != nil {
			return fmt.Errorf("logstore: encoding log %d: %w", i, err)
		}
		bodies[i] = b
	}

	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed {
		return ErrClosed
	}
	streams := make([]*stream, len(batch))
	for i := range batch {
		st, err := s.streamFor(&batch[i])
		if err != nil {
			return err
		}
		streams[i] = st
	}

	// One WAL record per ~8 MiB of bodies, numbered consecutively from the
	// next sequence number. Nothing is handed out until the WAL has it.
	base := s.seq + 1
	var recs []wal.Record
	for from := 0; from < len(bodies); {
		size, to := 0, from
		for to < len(bodies) && (to == from || size+len(bodies[to]) < maxWALRecord) {
			size += len(bodies[to]) + binary.MaxVarintLen64
			to++
		}
		recs = append(recs, wal.Record{Type: walRecordLogs, Data: encodeWALRecord(base+uint64(from), bodies[from:to])})
		from = to
	}
	if err := s.wal.Log(recs...); err != nil {
		return fmt.Errorf("logstore: WAL: %w", err)
	}
	if !s.opts.NoSync {
		if err := s.wal.Sync(); err != nil {
			return fmt.Errorf("logstore: syncing WAL: %w", err)
		}
	}
	seg := s.wal.Segment()
	now := s.clk.Now()

	s.smu.Lock()
	for i := range batch {
		streams[i].add(rawEntry{Ts: batch[i].Ts, Seq: base + uint64(i), Body: bodies[i]}, now, seg)
	}
	s.seq = base + uint64(len(batch)) - 1
	s.smu.Unlock()

	var full []*stream
	seen := map[*stream]bool{}
	for _, st := range streams {
		if !seen[st] && st.headBytes >= s.opts.BlockBytes {
			seen[st] = true
			full = append(full, st)
		}
	}
	for _, st := range full {
		if err := s.seal(st); err != nil {
			// The logs are durable in the WAL, so this is not a loss: the
			// head stays and the next seal retries.
			s.log.Error("sealing block", "stream", st.id, "error", err)
		}
	}
	return nil
}

func dayOf(ms int64) string { return time.UnixMilli(ms).UTC().Format(dayLayout) }

// seal turns a stream's head into blocks on disk: one block per day, because a
// chunk is a day. wmu must be held.
//
// One block per day is not a nicety. A chunk's day decides when retention
// deletes it, so a block holding a late log from last week beside fresh ones
// would be filed under last week's day and take the fresh logs with it when
// that day expired.
//
// The order is the point. Each block is written and fsynced *first*, then
// published (its entries removed from the head, the block added to the
// searchable list) in one step under smu, and only after every block may WAL
// segments that held the entries be deleted. A crash before a block's fsync
// leaves its entries in the WAL; a crash after leaves them in both, and
// recovery drops the WAL copy by sequence number. Any other order loses a log
// or searches one twice. Publishing day by day also makes a failure part-way
// harmless: the days already written are no longer in the head, so a retry
// writes only what is left.
func (s *Store) seal(st *stream) error {
	if len(st.head) == 0 {
		return nil
	}
	entries := append([]rawEntry(nil), st.head...)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Ts != entries[j].Ts {
			return entries[i].Ts < entries[j].Ts
		}
		return entries[i].Seq < entries[j].Seq
	})
	for from := 0; from < len(entries); {
		day := dayOf(entries[from].Ts)
		to := from
		for to < len(entries) && dayOf(entries[to].Ts) == day {
			to++
		}
		if err := s.sealDay(st, day, entries[from:to]); err != nil {
			return err
		}
		from = to
	}
	return s.truncateWAL()
}

// sealDay writes one day's entries of a stream as a block and publishes it.
func (s *Store) sealDay(st *stream, day string, group []rawEntry) error {
	meta, comp, err := encodeBlock(group)
	if err != nil {
		return err
	}
	cs, err := s.chunkFor(st, chunkKey{st.id, day})
	if err != nil {
		return err
	}
	pub, err := cs.w.appendBlock(meta, comp, buildBloom(group))
	if err != nil {
		return err
	}
	s.smu.Lock()
	cs.blocks = append(cs.blocks, pub)
	rest := st.head[:0:0]
	bytes := 0
	for _, e := range st.head {
		if dayOf(e.Ts) != day {
			rest = append(rest, e)
			bytes += len(e.Body) + 16
		}
	}
	st.head, st.headBytes = rest, bytes
	s.smu.Unlock()
	if s.afterBlock != nil {
		s.afterBlock()
	}
	return nil
}

// chunkFor returns the chunk a stream's next block goes in, opening it for
// appending. A stream appends to one chunk at a time: moving to another day
// seals the previous chunk's footer first, so closed days are complete.
func (s *Store) chunkFor(st *stream, key chunkKey) (*chunkState, error) {
	if st.open != nil && st.open.key != key {
		if err := st.open.closeWriter(); err != nil {
			return nil, err
		}
		st.open = nil
	}
	s.smu.RLock()
	cs := s.chunks[key]
	s.smu.RUnlock()
	if cs == nil {
		dir := filepath.Join(s.dir, key.day)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("logstore: %w", err)
		}
		cs = &chunkState{key: key, path: filepath.Join(dir, fmt.Sprintf("%d.chunk", key.stream))}
		s.smu.Lock()
		s.chunks[key] = cs
		s.smu.Unlock()
	}
	if cs.w == nil {
		w, err := openChunk(cs.path, s.opts.NoSync)
		if err != nil {
			return nil, err
		}
		cs.w = w
	}
	st.open = cs
	return cs, nil
}

func (cs *chunkState) closeWriter() error {
	if cs.w == nil {
		return nil
	}
	err := cs.w.seal()
	if cerr := cs.w.close(); err == nil {
		err = cerr
	}
	cs.w = nil
	return err
}

// truncateWAL drops WAL segments whose every entry is in a sealed block: all
// segments before the one holding the oldest entry still in any head. wmu held.
func (s *Store) truncateWAL() error {
	safe := s.wal.Segment()
	s.smu.RLock()
	for _, st := range s.byID {
		if len(st.head) > 0 {
			safe = min(safe, st.headSeg)
		}
	}
	s.smu.RUnlock()
	return wal.Truncate(s.walDir, safe, nil)
}

// Flush seals every head, so everything appended so far is in a chunk.
func (s *Store) Flush() error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return s.sealAll(func(*stream) bool { return true })
}

func (s *Store) sealAll(want func(*stream) bool) error {
	var errs []error
	s.smu.RLock()
	var todo []*stream
	for _, st := range s.byID {
		if len(st.head) > 0 && want(st) {
			todo = append(todo, st)
		}
	}
	s.smu.RUnlock()
	for _, st := range todo {
		if err := s.seal(st); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// loop seals heads that have sat too long and expires old days.
func (s *Store) loop() {
	defer close(s.done)
	t := s.clk.NewTicker(s.opts.Tick)
	defer t.Stop()
	var lastRetention time.Time
	for {
		select {
		case <-s.stop:
			return
		case <-t.C():
			// The clock's time, not the tick's: a tick that sat in its channel
			// while the loop was busy carries the time it was due, not now.
			now := s.clk.Now()
			s.wmu.Lock()
			if !s.closed {
				cutoff := now.Add(-s.opts.BlockAge)
				if err := s.sealAll(func(st *stream) bool { return !st.headSince.After(cutoff) }); err != nil {
					s.log.Error("sealing aged heads", "error", err)
				}
				if s.opts.Retention > 0 && now.Sub(lastRetention) >= time.Minute {
					lastRetention = now
					if err := s.expire(now); err != nil {
						s.log.Error("expiring chunks", "error", err)
					}
				}
			}
			s.wmu.Unlock()
		}
	}
}

// expire deletes whole days that ended more than Retention ago. wmu held.
func (s *Store) expire(now time.Time) error {
	cutoff := now.Add(-s.opts.Retention)
	s.smu.RLock()
	var doomed []*chunkState
	for k, cs := range s.chunks {
		day, err := time.Parse(dayLayout, k.day)
		if err == nil && !day.Add(24*time.Hour).After(cutoff) {
			doomed = append(doomed, cs)
		}
	}
	s.smu.RUnlock()
	var errs []error
	for _, cs := range doomed {
		s.smu.Lock()
		delete(s.chunks, cs.key)
		if st := s.byID[cs.key.stream]; st != nil && st.open == cs {
			st.open = nil
		}
		s.smu.Unlock()
		_ = cs.closeWriter()
		if err := os.Remove(cs.path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
		_ = os.Remove(filepath.Dir(cs.path)) // only succeeds once the day is empty
	}
	return errors.Join(errs...)
}

// Close seals every head, writes every footer and closes the WAL and catalog.
func (s *Store) Close() error {
	s.wmu.Lock()
	if s.closed {
		s.wmu.Unlock()
		return nil
	}
	s.closed = true
	close(s.stop)
	s.wmu.Unlock()
	<-s.done

	s.wmu.Lock()
	defer s.wmu.Unlock()
	errs := []error{s.sealAll(func(*stream) bool { return true })}
	s.smu.RLock()
	for _, cs := range s.chunks {
		errs = append(errs, cs.closeWriter())
	}
	s.smu.RUnlock()
	errs = append(errs, s.truncateWAL(), s.wal.Close(), s.cat.close())
	return errors.Join(errs...)
}

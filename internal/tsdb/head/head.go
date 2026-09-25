package head

import (
	"errors"
	"fmt"
	"hash/maphash"
	"math"
	"sort"
	"sync"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/internal/tsdb/chunkenc"
	"github.com/tuvo1106/ozymandias/internal/tsdb/index"
	"github.com/tuvo1106/ozymandias/internal/tsdb/wal"
)

// Rejections. Each is a policy decision about data that cannot be stored as
// given, and each is counted rather than hidden: a silent drop is how a
// monitoring system lies.
var (
	// ErrOutOfOrder: the sample is older than the newest one in its series.
	// Chunks are append-only, so accepting it is not possible without
	// rewriting history.
	ErrOutOfOrder = errors.New("head: sample is out of order")
	// ErrOutOfBounds: the sample belongs to a time range already written to a
	// block. The head cannot take it and the block cannot be edited.
	ErrOutOfBounds = errors.New("head: sample is before the head's valid range")
	// ErrSeriesLimit: this metric already has max_series_per_metric series.
	// Cardinality protection — the alternative is an unbounded index.
	ErrSeriesLimit = errors.New("head: series limit for this metric reached")
)

const (
	// stripes shard the series map. Sharding is what keeps concurrent
	// appenders from serializing on one lock; 256 is enough that contention
	// is rare and small enough that the locks themselves are cheap.
	stripes = 256
	// DefaultMaxSeriesPerMetric bounds cardinality per metric name.
	DefaultMaxSeriesPerMetric = 10_000
	// DefaultBlockRange is how much time one block covers.
	DefaultBlockRange = int64(2 * 60 * 60 * 1000) // 2h in ms
)

// Sample is one point for one series, identified by the id the head assigned.
type Sample struct {
	ID uint64
	T  int64
	V  float64
}

// Logger is the write-ahead log the head records to. It is an interface so a
// head can run without durability in tests, and so the same head works over
// M7's queue-backed log.
type Logger interface {
	Log(recs ...wal.Record) error
	Sync() error
}

// Options configure a head.
type Options struct {
	// WAL receives series and sample records before they are visible. Nil
	// means no durability: acceptable only in tests.
	WAL Logger
	// BlockRange is the width of a block in ms; chunks are cut at its
	// boundaries so a block cut never has to split one.
	BlockRange int64
	// MaxSeriesPerMetric bounds cardinality. 0 uses the default; negative
	// means unlimited.
	MaxSeriesPerMetric int
	// SyncOnAppend fsyncs the WAL before Append returns. The default (false)
	// is group commit: durable within the sync interval, much faster.
	SyncOnAppend bool
}

// Head is the in-memory window of recent samples: everything not yet written
// to a block. It owns series identity — ids are assigned here — and the
// inverted index over live series.
type Head struct {
	opts       Options
	seed       maphash.Seed
	shards     [stripes]shard
	logBuf     []byte
	mu         sync.RWMutex // guards index, ids, counters and minValidTime
	postings   *index.MemPostings
	byID       map[uint64]*memSeries
	perMetric  map[string]int
	nextID     uint64
	minValid   int64 // samples before this belong to a block already written
	logMu      sync.Mutex
	oooDropped int64
	limitHits  int64
}

type shard struct {
	mu     sync.RWMutex
	series map[string]*memSeries
}

// memSeries is one series' live chunks.
type memSeries struct {
	mu     sync.RWMutex
	id     uint64
	ref    tsdb.SeriesRef
	chunks []*memChunk
	app    *chunkenc.Appender
	lastT  int64
	lastV  float64
	// logged records whether this series' definition is in the log. It is
	// guarded by Head.logMu, not by mu, because the question it answers is
	// about the log's contents and only the goroutine holding logMu may
	// change them. Creating the series and logging it are deliberately
	// separate: see Append.
	logged bool
}

type memChunk struct {
	minT, maxT int64
	chunk      *chunkenc.Chunk
}

// New returns an empty head.
func New(opts Options) *Head {
	if opts.BlockRange <= 0 {
		opts.BlockRange = DefaultBlockRange
	}
	if opts.MaxSeriesPerMetric == 0 {
		opts.MaxSeriesPerMetric = DefaultMaxSeriesPerMetric
	}
	h := &Head{
		opts:      opts,
		seed:      maphash.MakeSeed(),
		postings:  index.NewMemPostings(),
		byID:      map[uint64]*memSeries{},
		perMetric: map[string]int{},
		minValid:  math.MinInt64,
	}
	for i := range h.shards {
		h.shards[i].series = map[string]*memSeries{}
	}
	return h
}

// Rejection is one sample the head refused, identified by its position in the
// batch. Callers need the position, not a parsed error message, to attribute a
// refusal back to whatever they built the batch from.
type Rejection struct {
	Index int
	Err   error
}

// Append adds samples for the given series, assigning ids as needed.
//
// stored[i] reports whether samples[i] was actually written. A sample the head
// already holds is accepted — that is how a retried batch stays idempotent —
// but it stored nothing, and a caller counting what it wrote must be able to
// tell the difference. It is nil when the whole batch failed.
//
// The batch is written to the WAL (and synced, if configured) *before* the
// samples become visible, so nothing can be read that would not survive a
// crash. Per-sample rejections are returned alongside rather than failing the
// batch: one bad series must not discard another's data. An error that is not
// a per-sample rejection — a failed log write — comes back with Index -1, and
// nothing in the batch was applied.
func (h *Head) Append(refs []tsdb.SeriesRef, samples []Sample) (stored []bool, rejected []Rejection) {
	if len(refs) != len(samples) {
		return nil, []Rejection{{Index: -1,
			Err: fmt.Errorf("head: %d refs for %d samples", len(refs), len(samples))}}
	}
	// Resolving, logging and applying are one critical section: the commit.
	//
	// They have to be, and each boundary was a bug before it was a comment.
	// Logging and applying together is what makes WAL order *be* apply order —
	// otherwise two appends racing on one series are logged in one order and
	// applied in the other, and replay reconstructs data the head never had.
	// Resolving inside it too is what makes the series pointer trustworthy:
	// [Head.Truncate] forgets series that a block has taken over, and a
	// resolution from before it ran hands this loop a series that has been
	// removed from every index, so the samples would be stored where no query
	// can reach them.
	//
	// What this costs is that appends to the head are serial. They already
	// were, in every configuration that matters: with a WAL the fsync inside
	// this section dwarfs everything else, and a batch amortizes it over
	// thousands of samples. The 256 stripes still earn their place on the read
	// path and by spreading the map, just not by letting two appends commit at
	// once.
	h.logMu.Lock()
	defer h.logMu.Unlock()

	resolved := make([]*memSeries, len(samples))
	ok := make([]bool, len(samples))
	for i := range samples {
		ms, _, err := h.getOrCreate(refs[i])
		if err != nil {
			rejected = append(rejected, Rejection{Index: i, Err: err})
			continue
		}
		samples[i].ID = ms.id
		resolved[i] = ms
		ok[i] = true
	}
	valid := samples[:0:0]
	for i, s := range samples {
		if ok[i] {
			valid = append(valid, s)
		}
	}
	if len(valid) == 0 {
		return make([]bool, len(samples)), rejected
	}
	if h.opts.WAL != nil {
		// Which series still need a definition record is decided here and not
		// by whoever created them: a series can be created by one appender and
		// first *logged* by another, and if the record were tied to creation,
		// samples could reach the log ahead of the definition they point at.
		// The flag is only set once the write lands, so a failed log leaves
		// nothing half-declared.
		var pending []*memSeries
		for i, ms := range resolved {
			if !ok[i] || ms.logged {
				continue
			}
			ms.logged = true // also dedupes a batch mentioning it twice
			pending = append(pending, ms)
		}
		newSeries := make([]seriesRecord, 0, len(pending))
		for _, ms := range pending {
			newSeries = append(newSeries, seriesRecord{id: ms.id, ref: ms.ref})
		}
		if err := h.logLocked(newSeries, valid); err != nil {
			for _, ms := range pending {
				ms.logged = false
			}
			return nil, append(rejected, Rejection{Index: -1, Err: err})
		}
	}
	// Only now make them visible.
	stored = make([]bool, len(samples))
	for i, s := range samples {
		if !ok[i] {
			continue
		}
		wrote, err := h.appendTo(resolved[i], s.T, s.V)
		if err != nil {
			if errors.Is(err, ErrOutOfOrder) || errors.Is(err, ErrOutOfBounds) {
				h.mu.Lock()
				h.oooDropped++
				h.mu.Unlock()
			}
			rejected = append(rejected, Rejection{Index: i, Err: err})
			continue
		}
		stored[i] = wrote
	}
	return stored, rejected
}

type seriesRecord struct {
	id  uint64
	ref tsdb.SeriesRef
}

// maxSampleBytes is the most one encoded sample can occupy: a uvarint id and
// a varint timestamp are at most ten bytes each, and the value is a fixed
// eight.
const maxSampleBytes = 10 + 10 + 8

// maxSamplesPerRecord is how many samples certainly fit in one WAL record,
// leaving room for the count prefix.
//
// A batch larger than this is not exotic. Intake accepts a 16 MiB
// decompressed body (wire.MaxDecompressedBytes) and caps series per request
// but not *points* per request, so one dense body is around 1.1 million
// points — comfortably over. Encoding that as a single record put it past
// wal.MaxRecordSize, and wal.Log rejects an oversized record before writing
// anything, so Append failed the whole batch with an Index -1 error. Nothing
// was corrupted and nothing was lost; the request simply could never succeed,
// and every retry of the same body failed identically.
const maxSamplesPerRecord = (wal.MaxRecordSize - 10) / maxSampleBytes

// logLocked writes one batch's records. The caller holds logMu and keeps
// holding it until the samples are applied; see Append.
//
// A batch too large for one record is split across several. They go to
// wal.Log in a single call, which is what keeps the split invisible: the log
// writes them consecutively under its own lock and undoes a partial write, so
// replay sees the same samples in the same order it would have seen from one
// record. Splitting into separate Log calls would not be equivalent — another
// appender could interleave its records between the halves.
func (h *Head) logLocked(series []seriesRecord, samples []Sample) error {
	recs := make([]wal.Record, 0, len(series)+1+len(samples)/maxSamplesPerRecord)
	for _, s := range series {
		h.logBuf = encodeSeries(h.logBuf[:0], s.id, s.ref)
		recs = append(recs, wal.Record{Type: RecordSeries, Data: append([]byte(nil), h.logBuf...)})
	}
	for rest := samples; ; {
		chunk := rest
		if len(chunk) > maxSamplesPerRecord {
			chunk = chunk[:maxSamplesPerRecord]
		}
		h.logBuf = encodeSamples(h.logBuf[:0], chunk)
		recs = append(recs, wal.Record{Type: RecordSamples, Data: append([]byte(nil), h.logBuf...)})
		if rest = rest[len(chunk):]; len(rest) == 0 {
			break
		}
	}
	if err := h.opts.WAL.Log(recs...); err != nil {
		return fmt.Errorf("head: logging: %w", err)
	}
	if h.opts.SyncOnAppend {
		if err := h.opts.WAL.Sync(); err != nil {
			return fmt.Errorf("head: syncing: %w", err)
		}
	}
	return nil
}

// appendTo applies the head's ordering rules and stores the sample. It reports
// whether anything was actually stored: a sample the series already holds is
// accepted without being stored, and a caller counting what it wrote must not
// count it.
func (h *Head) appendTo(ms *memSeries, t int64, v float64) (stored bool, err error) {
	h.mu.RLock()
	minValid, blockRange := h.minValid, h.opts.BlockRange
	h.mu.RUnlock()

	ms.mu.Lock()
	defer ms.mu.Unlock()

	if t < minValid {
		return false, ErrOutOfBounds
	}
	if len(ms.chunks) > 0 && t <= ms.lastT {
		// A sample the series already holds, with the same value, is what a
		// retried batch looks like: it carries no new information, so accept
		// it as a no-op and keep at-least-once delivery idempotent. Anything
		// else at or before the newest sample is a real ordering violation.
		//
		// The check decodes a chunk, which is why it is here and not before
		// the comparison above: the accept path never reaches it, and a
		// rejection is rare enough to afford it. Checking only the newest
		// sample would be cheaper and wrong — an agent resends a whole batch,
		// not its last point.
		if ms.holds(t, v) {
			return false, nil
		}
		return false, ErrOutOfOrder
	}
	cur := ms.head()
	// Cut at the sample cap, and at a block boundary so that cutting a block
	// never has to split a chunk in half.
	if cur == nil || cur.chunk.Full() ||
		tsdb.BlockOf(t, blockRange) != tsdb.BlockOf(cur.minT, blockRange) {
		c := chunkenc.NewChunk()
		app, err := c.Appender()
		if err != nil {
			return false, err
		}
		cur = &memChunk{minT: t, maxT: t, chunk: c}
		ms.chunks = append(ms.chunks, cur)
		ms.app = app
	}
	if err := ms.app.Append(t, v); err != nil {
		return false, err
	}
	cur.maxT = t
	ms.lastT, ms.lastV = t, v
	return true, nil
}

// holds reports whether the series already stores exactly (t, v). The caller
// holds ms.mu.
func (ms *memSeries) holds(t int64, v float64) bool {
	for _, c := range ms.chunks {
		if t < c.minT || t > c.maxT {
			continue
		}
		it := c.chunk.Iterator()
		for it.Next() {
			ct, cv := it.At()
			if ct > t {
				break // samples ascend; t is not in this chunk after all
			}
			if ct == t {
				return cv == v
			}
		}
		// A corrupt chunk here would be a bug in this process's own memory.
		// Reporting the sample as absent is the safe reading: it becomes a
		// rejection, not a silent accept of data we did not verify.
		return false
	}
	return false
}

func (ms *memSeries) head() *memChunk {
	if len(ms.chunks) == 0 {
		return nil
	}
	return ms.chunks[len(ms.chunks)-1]
}

func (h *Head) shardFor(key string) *shard {
	var hash maphash.Hash
	hash.SetSeed(h.seed)
	_, _ = hash.WriteString(key)
	return &h.shards[hash.Sum64()%stripes]
}

// getOrCreate finds a series by identity, creating and indexing it if new.
func (h *Head) getOrCreate(ref tsdb.SeriesRef) (*memSeries, bool, error) {
	key := ref.Key()
	sh := h.shardFor(key)

	sh.mu.RLock()
	ms, ok := sh.series[key]
	sh.mu.RUnlock()
	if ok {
		return ms, false, nil
	}

	sh.mu.Lock()
	if ms, ok = sh.series[key]; ok { // lost the race; fine
		sh.mu.Unlock()
		return ms, false, nil
	}
	h.mu.Lock()
	if h.opts.MaxSeriesPerMetric > 0 && h.perMetric[ref.Metric] >= h.opts.MaxSeriesPerMetric {
		h.limitHits++
		h.mu.Unlock()
		sh.mu.Unlock()
		return nil, false, ErrSeriesLimit
	}
	h.nextID++
	id := h.nextID
	ms = &memSeries{id: id, ref: ref}
	h.byID[id] = ms
	h.perMetric[ref.Metric]++
	h.postings.Add(id, ref)
	h.mu.Unlock()

	sh.series[key] = ms
	sh.mu.Unlock()
	return ms, true, nil
}

func (h *Head) series(id uint64) *memSeries {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.byID[id]
}

// MinValidTime is the oldest timestamp the head will accept.
func (h *Head) MinValidTime() int64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.minValid
}

// Stats describes the head's size and what it has refused.
type Stats struct {
	Series       int64
	Chunks       int64
	Samples      int64
	TagPairs     int64
	OOORejected  int64
	LimitRejects int64
	MinT, MaxT   int64
}

// Stats snapshots the head.
// Stats are a snapshot, and a slightly blurred one: the series are listed
// under the lock and then walked without it, so a series created during the
// walk is not counted and contributes nothing to MinT or MaxT. That is the
// right trade for a counter read on every self-metrics tick, but it makes
// these numbers unsafe to derive a boundary from: the block cut in the db
// package uses MinT to decide *whether* to cut, never to decide what the cut
// covers.
func (h *Head) Stats() Stats {
	h.mu.RLock()
	ids := make([]uint64, 0, len(h.byID))
	for id := range h.byID {
		ids = append(ids, id)
	}
	series, pairs := h.postings.Size()
	st := Stats{
		Series: int64(series), TagPairs: int64(pairs),
		OOORejected: h.oooDropped, LimitRejects: h.limitHits,
		MinT: math.MinInt64, MaxT: math.MinInt64,
	}
	h.mu.RUnlock()

	first := true
	for _, id := range ids {
		ms := h.series(id)
		if ms == nil {
			continue
		}
		ms.mu.RLock()
		st.Chunks += int64(len(ms.chunks))
		for _, c := range ms.chunks {
			st.Samples += int64(c.chunk.NumSamples())
			if first || c.minT < st.MinT {
				st.MinT = c.minT
			}
			if first || c.maxT > st.MaxT {
				st.MaxT = c.maxT
			}
			first = false
		}
		ms.mu.RUnlock()
	}
	return st
}

// Series returns the ref for an id, for tests and for block writing.
func (h *Head) Series(id uint64) (tsdb.SeriesRef, bool) {
	ms := h.series(id)
	if ms == nil {
		return tsdb.SeriesRef{}, false
	}
	return ms.ref, true
}

// Lookup returns a read view of the head's index that takes the head's lock on
// every call, for callers outside the head — the metadata queries.
//
// This exists because [Head.Postings] hands out the live index, and the head
// mutates that index every time a series is created. A metadata query holding
// the bare pointer is a map read racing a map write, which Go does not turn
// into a wrong answer but into a fatal error that takes the process down. The
// read path has to go through a lock, and the type that owns the lock is the
// one that should be taking it.
func (h *Head) Lookup() index.Lookup { return headLookup{h} }

// headLookup is [Head.Lookup]'s implementation: every method is the index's,
// under the head's read lock. The lock is taken per call rather than held
// across a whole query because each of these returns a complete answer — a
// metadata query that saw a series appear halfway through is not wrong, it is
// just a moment later.
type headLookup struct{ h *Head }

func (l headLookup) Postings(key, value string) []uint64 {
	l.h.mu.RLock()
	defer l.h.mu.RUnlock()
	return l.h.postings.Postings(key, value)
}

func (l headLookup) TagValues(key string) []string {
	l.h.mu.RLock()
	defer l.h.mu.RUnlock()
	return l.h.postings.TagValues(key)
}

func (l headLookup) TagKeys() []string {
	l.h.mu.RLock()
	defer l.h.mu.RUnlock()
	return l.h.postings.TagKeys()
}

func (l headLookup) AllSeries() []uint64 {
	l.h.mu.RLock()
	defer l.h.mu.RUnlock()
	return l.h.postings.AllSeries()
}

// Postings exposes the live index. It is **not** safe to call from another
// goroutine while the head is being appended to: the caller is responsible for
// the head's lock, which it has no way to take. Anything outside this package
// wants [Head.Lookup] instead; this remains for tests, which are
// single-threaded, and for the head's own use.
func (h *Head) Postings() *index.MemPostings {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.postings
}

// Select returns the series matching sel that have samples in [from, to],
// sorted by series key so results are deterministic.
//
// It returns an error if a chunk fails to decode, rather than the samples that
// did decode. A short read is indistinguishable from a series that genuinely
// holds fewer samples, and [Head.Select] is what a block cut is built from:
// the caller writes the block and then truncates the head and the log to
// match, so a silently short answer here is silently lost data in both
// durable copies. [github.com/tuvo1106/ozymandias/internal/tsdb/block.Block]
// makes the same promise.
func (h *Head) Select(sel tsdb.Selector, from, to int64) ([]tsdb.SeriesSamples, error) {
	h.mu.RLock()
	ids := h.postings.Select(sel)
	h.mu.RUnlock()

	out := make([]tsdb.SeriesSamples, 0, len(ids))
	for _, id := range ids {
		ms := h.series(id)
		if ms == nil {
			continue
		}
		samples, err := ms.samplesIn(from, to)
		if err != nil {
			return nil, fmt.Errorf("head: reading series %s: %w", ms.ref.Key(), err)
		}
		if len(samples) == 0 {
			continue
		}
		out = append(out, tsdb.SeriesSamples{Series: ms.ref, Samples: samples})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Series.Key() < out[j].Series.Key() })
	return out, nil
}

func (ms *memSeries) samplesIn(from, to int64) ([]tsdb.Sample, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	var out []tsdb.Sample
	for _, c := range ms.chunks {
		if c.maxT < from || c.minT > to {
			continue
		}
		it := c.chunk.Iterator()
		for it.Next() {
			t, v := it.At()
			if t < from || t > to {
				continue
			}
			out = append(out, tsdb.Sample{T: t, V: v})
		}
		if err := it.Err(); err != nil {
			// A corrupt in-memory chunk is a bug, not bad input — but the
			// caller cannot tell a short read from a short series, and the
			// block cut that reads this then truncates the head and the log
			// to match. Stopping at the bad chunk used to drop it and every
			// later chunk of this series from the block and from both durable
			// copies, quietly. Refuse the read instead.
			return nil, fmt.Errorf("decoding chunk [%d, %d]: %w", c.minT, c.maxT, err)
		}
	}
	return out, nil
}

// Freeze refuses everything before mint from now on, without dropping
// anything. It is the first half of a block cut.
//
// Cutting a block is: decide the boundary, copy the samples below it to disk,
// then forget them. Raising the bound only at the end leaves a window — the
// length of a block write — in which a sample below the boundary is still
// accepted, is not in the snapshot being written, and is then dropped by
// [Head.Truncate]. It was acknowledged and it is gone, which is the one thing
// a store must never do. Freezing first converts that silent loss into an
// honest ErrOutOfBounds for the handful of samples involved: they were about
// to be refused anyway, a moment later.
//
// It takes logMu, so it cannot land in the middle of a batch being applied: a
// sample that was accepted before the freeze is in the head, visible to the
// snapshot that follows, and one accepted after is above the boundary.
func (h *Head) Freeze(mint int64) {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	if mint > h.minValid {
		h.minValid = mint
	}
}

// Truncate drops chunks that end before mint and forgets series left with
// none, after a block covering that range has been written.
//
// This is the head's garbage collection: without it the head grows forever
// and the WAL can never be truncated either.
//
// It holds logMu for the whole pass, which is what makes forgetting a series
// safe. Dropping a series means removing it from the shard map, the id map and
// the postings; an appender that had already resolved that series still holds
// the pointer, and would append into an object nothing can find any more —
// samples acknowledged and then invisible. Holding the commit lock means no
// append is anywhere between resolving a series and applying to it. The pause
// is one pass over the head's series, once per block cut.
func (h *Head) Truncate(mint int64) (droppedSeries, droppedChunks int) {
	h.logMu.Lock()
	defer h.logMu.Unlock()

	h.mu.Lock()
	h.minValid = mint
	ids := make([]uint64, 0, len(h.byID))
	for id := range h.byID {
		ids = append(ids, id)
	}
	h.mu.Unlock()

	for _, id := range ids {
		ms := h.series(id)
		if ms == nil {
			continue
		}
		ms.mu.Lock()
		kept := ms.chunks[:0]
		for _, c := range ms.chunks {
			if c.maxT >= mint {
				kept = append(kept, c)
				continue
			}
			droppedChunks++
		}
		// The appender points into the newest chunk; if that chunk is gone the
		// next append must start a fresh one.
		if len(kept) != len(ms.chunks) && len(kept) == 0 {
			ms.app = nil
		}
		ms.chunks = kept
		empty := len(ms.chunks) == 0
		ref := ms.ref
		ms.mu.Unlock()

		if !empty {
			continue
		}
		sh := h.shardFor(ref.Key())
		sh.mu.Lock()
		delete(sh.series, ref.Key())
		sh.mu.Unlock()

		h.mu.Lock()
		delete(h.byID, id)
		h.postings.Delete(id, ref)
		if n := h.perMetric[ref.Metric] - 1; n <= 0 {
			delete(h.perMetric, ref.Metric)
		} else {
			h.perMetric[ref.Metric] = n
		}
		h.mu.Unlock()
		droppedSeries++
	}
	return droppedSeries, droppedChunks
}

// KeepForCheckpoint returns the WAL truncation policy for a TSDB log whose
// head refuses samples before minValid — normally [Head.MinValidTime] taken
// straight after the block cut that prompted the truncation.
//
// Passing a minValid that is too high silently deletes acknowledged data, so
// it is worth saying what the number means: everything below it is in a block,
// everything at or above it is in the head and the log is its only durable
// copy.
func KeepForCheckpoint(minValid int64) func(wal.Record) bool {
	return keepForCheckpoint(minValid)
}

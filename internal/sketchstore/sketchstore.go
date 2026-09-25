package sketchstore

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/internal/sketch"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// Key prefixes. One byte, so the two keyspaces sort apart and a range scan
// over one never sees the other.
const (
	// prefixPoint: 's' | seriesID u64 BE | ts u32 BE → the encoded sketch.
	// Big-endian so byte order is numeric order, which is what makes a range
	// scan over one series' time window a contiguous read.
	prefixPoint = 's'
	// prefixSeries: 'x' | seriesID u64 BE → the series' canonical key.
	// It is what makes retention possible (the ids to sweep) and what turns
	// an id collision from silent corruption into a rejection.
	prefixSeries = 'x'
)

const (
	pointKeyLen  = 1 + 8 + 4
	seriesKeyLen = 1 + 8
)

// MaxTimestamp is the newest bucket start the key format can hold: the key
// carries unix seconds in 32 bits, which runs out in 2106.
const MaxTimestamp = int64(math.MaxUint32)

// ErrIDCollision means two different series hashed to the same id. See
// [SeriesID].
var ErrIDCollision = errors.New("sketchstore: series id collision")

// SeriesID is the 64-bit id a series is stored under: FNV-1a over its
// canonical key.
//
// A hash rather than an assigned number, because the alternative is a second
// source of truth for series identity that has to be kept in step with the
// TSDB's across restarts, compactions and a head that forgets a series when
// it truncates. A hash needs no coordination: given a [tsdb.SeriesRef], any
// process can compute where its sketches live.
//
// The cost is that two series can collide — about one chance in 37 million at
// a hundred thousand series. The store keeps the canonical key beside the id
// so it can tell, and a collision is a rejection with a name in it rather
// than two metrics quietly sharing a percentile.
func SeriesID(ref tsdb.SeriesRef) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(ref.Key()))
	return h.Sum64()
}

// Point is one sketch at one bucket start.
type Point struct {
	// TimeMs is the bucket start in unix milliseconds, the same unit every
	// other store in the project uses. It is stored as seconds.
	TimeMs int64
	Sketch *sketch.Sketch
}

// Entry is one series' sketches, for [Store.Append].
type Entry struct {
	Series tsdb.SeriesRef
	Points []Point
}

// AppendResult reports what Append stored. Per-series problems come back
// here rather than as an error, so one bad series does not fail a batch —
// the same contract [tsdb.MetricStore] has.
type AppendResult struct {
	Series, Points int
	Rejected       []tsdb.Rejected
}

// Stats is the store's size, for self-metrics and the UI.
type Stats struct {
	Series    int64
	DiskBytes int64
}

// Options configures a Store.
type Options struct {
	// Dir is the Pebble directory. Created if absent.
	Dir string
	// Retention deletes sketches older than this. Negative keeps everything;
	// zero takes the default (15 days), matching the TSDB's.
	Retention time.Duration
	// Grace holds sketches past the retention cutoff.
	//
	// The two stores round the window differently and cannot be made to
	// agree: the TSDB drops a whole block once its *newest* sample is older
	// than retention, so a sample survives for up to one block range past
	// the cutoff, while a sketch is a single key that goes exactly on it.
	// Whichever way that gap falls, one store outlives the other — the only
	// choice is which.
	//
	// Sketches outliving counts is the harmless direction. A sketch is only
	// ever reached through its `<metric>.count` series, so one whose count
	// is gone is invisible: it costs disk until the next sweep and nothing
	// else. The other way round is a visible wrong answer — the count chart
	// draws a line and p95 returns null over the same minutes. So set this
	// to the TSDB's block range and let the sketches lag.
	Grace    time.Duration
	Clock    clock.Clock
	Logger   *slog.Logger
	Registry *selfmetrics.Registry
}

// Store keeps one DDSketch per series per bucket in Pebble.
//
// Why a separate store at all: a sketch is a kilobyte of buckets, not a
// float64, so the TSDB's chunk encoding — which exists to spend a handful of
// bits on a sample whose value barely moved — has nothing to offer it. What
// the two do share is *identity*: a sketch series is selected through the
// TSDB's index like any other, by way of the `<metric>.count` series the
// intake writes beside every sketch. See docs/adr/ADR-0015.
//
// Safe for concurrent use.
type Store struct {
	db        *pebble.DB
	retention time.Duration
	grace     time.Duration
	clock     clock.Clock
	log       *slog.Logger

	// sweepMu keeps Truncate away from in-flight appends. Retention decides
	// a series is empty, then deletes its index entry; an append that lands
	// between those two steps writes points and — finding the id already
	// known — does not rewrite the index entry that is about to be deleted.
	// The series is then points with no index, which no later sweep finds.
	// Appends hold it shared, so they still run concurrently with each other.
	sweepMu sync.RWMutex

	// mu guards ids, the canonical key of every series the store holds. It
	// is loaded at Open and is what retention sweeps and what collisions are
	// detected against. Never taken while holding sweepMu's write lock for
	// anything but a map operation.
	mu  sync.RWMutex
	ids map[uint64]string

	closeOnce sync.Once
	// closed is read by Stats, which outlives the store: the gauges built on
	// it stay registered on the server's registry, and Pebble panics on any
	// operation after Close. A scrape arriving during shutdown must get a
	// number, not take the process down with it.
	closed atomic.Bool

	appended, rejected, collisions *selfmetrics.Counter
}

// Open opens or creates the store under opts.Dir.
func Open(opts Options) (*Store, error) {
	if opts.Clock == nil {
		opts.Clock = clock.Real()
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Registry == nil {
		opts.Registry = selfmetrics.NewRegistry()
	}
	if opts.Retention == 0 {
		opts.Retention = 15 * 24 * time.Hour
	}
	if opts.Grace < 0 {
		opts.Grace = 0
	}
	db, err := pebble.Open(opts.Dir, &pebble.Options{Logger: pebbleLogger{opts.Logger}})
	if err != nil {
		return nil, fmt.Errorf("sketchstore: opening %s: %w", opts.Dir, err)
	}
	s := &Store{
		db:         db,
		retention:  opts.Retention,
		grace:      opts.Grace,
		clock:      opts.Clock,
		log:        opts.Logger,
		ids:        map[uint64]string{},
		appended:   opts.Registry.Counter("ozy.sketchstore.points_appended"),
		rejected:   opts.Registry.Counter("ozy.sketchstore.series_rejected"),
		collisions: opts.Registry.Counter("ozy.sketchstore.id_collisions"),
	}
	if err := s.loadIDs(); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	opts.Registry.GaugeFunc("ozy.sketchstore.series", func() float64 { return float64(s.Stats().Series) })
	opts.Registry.GaugeFunc("ozy.sketchstore.disk_bytes", func() float64 { return float64(s.Stats().DiskBytes) })
	return s, nil
}

// loadIDs reads the series keyspace into memory. It is one entry per series,
// not per point, so it stays small enough to hold: a hundred thousand series
// is a few megabytes.
func (s *Store) loadIDs() (err error) {
	it, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: []byte{prefixSeries},
		UpperBound: []byte{prefixSeries + 1},
	})
	if err != nil {
		return fmt.Errorf("sketchstore: reading the series index: %w", err)
	}
	// An iterator reports read errors from Close as well as from Error, so
	// both are joined into the result rather than one being trusted.
	defer func() { err = errors.Join(err, it.Close()) }()
	for it.First(); it.Valid(); it.Next() {
		k := it.Key()
		if len(k) != seriesKeyLen {
			return fmt.Errorf("sketchstore: series index holds a %d-byte key", len(k))
		}
		s.ids[binary.BigEndian.Uint64(k[1:])] = string(it.Value())
	}
	return it.Error()
}

func pointKey(id uint64, tsSec int64) []byte {
	k := make([]byte, pointKeyLen)
	k[0] = prefixPoint
	binary.BigEndian.PutUint64(k[1:], id)
	binary.BigEndian.PutUint32(k[9:], uint32(tsSec)) //nolint:gosec // bounded by the caller
	return k
}

// seriesEnd is the exclusive upper bound covering every point of one series.
// It is a point key with the highest timestamp plus a trailing byte, which
// sorts after every 13-byte key with the same id — and unlike "the next id"
// it cannot overflow at the top of the keyspace.
func seriesEnd(id uint64) []byte {
	return append(pointKey(id, MaxTimestamp), 0xff)
}

func seriesKey(id uint64) []byte {
	k := make([]byte, seriesKeyLen)
	k[0] = prefixSeries
	binary.BigEndian.PutUint64(k[1:], id)
	return k
}

// Append stores every point of every entry. A series whose identity the
// store cannot use — a non-canonical ref, or one colliding with another
// series' id — is rejected whole and named in the result.
func (s *Store) Append(ctx context.Context, entries []Entry) (res AppendResult, err error) {
	if len(entries) == 0 {
		return res, nil
	}
	s.sweepMu.RLock()
	defer s.sweepMu.RUnlock()

	batch := s.db.NewBatch()
	defer func() { err = errors.Join(err, batch.Close()) }()

	newIDs := map[uint64]string{}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		id, err := s.claim(e.Series, newIDs)
		if err != nil {
			res.Rejected = append(res.Rejected, tsdb.Rejected{Series: e.Series, Reason: err.Error()})
			continue
		}
		kvs, err := encodePoints(id, e.Points)
		if err != nil {
			res.Rejected = append(res.Rejected, tsdb.Rejected{Series: e.Series, Reason: err.Error()})
			continue
		}
		// The index entry goes in before the points it indexes, so a batch
		// that is only partly applied can leave an index entry with nothing
		// under it — which retention sweeps — but never points with no index
		// entry, which it cannot.
		if key := e.Series.Key(); s.known(id) != key && newIDs[id] != key {
			if err := batch.Set(seriesKey(id), []byte(key), nil); err != nil {
				return res, fmt.Errorf("sketchstore: recording series %s: %w", key, err)
			}
			newIDs[id] = key
		}
		for _, kv := range kvs {
			if err := batch.Set(kv.key, kv.value, nil); err != nil {
				return res, fmt.Errorf("sketchstore: writing %s: %w", e.Series.Key(), err)
			}
		}
		res.Series++
		res.Points += len(kvs)
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return AppendResult{}, fmt.Errorf("sketchstore: committing %d series: %w", res.Series, err)
	}
	s.mu.Lock()
	for id, key := range newIDs {
		s.ids[id] = key
	}
	s.mu.Unlock()
	s.appended.Add(int64(res.Points))
	s.rejected.Add(int64(len(res.Rejected)))
	return res, nil
}

// encodePoints turns one series' points into the keys and values they will
// be written under, or fails without having written anything.
//
// Encoding first and writing second is the whole point. A loop that wrote as
// it encoded would leave the points before a failure committed with the rest
// of the batch while its caller skipped the series-index entry — points no
// sweep would ever find, because retention walks the index. Nothing is
// half-written if nothing is written until everything is encoded.
func encodePoints(id uint64, points []Point) ([]keyValue, error) {
	out := make([]keyValue, 0, len(points))
	for _, p := range points {
		if p.Sketch == nil {
			return nil, errors.New("nil sketch")
		}
		sec := p.TimeMs / 1000
		if p.TimeMs < 0 || sec > MaxTimestamp {
			return nil, fmt.Errorf("timestamp %dms is outside what the key format holds (unix seconds in 32 bits)", p.TimeMs)
		}
		v, err := encodeValue(p.Sketch)
		if err != nil {
			return nil, err
		}
		out = append(out, keyValue{key: pointKey(id, sec), value: v})
	}
	return out, nil
}

// keyValue is one encoded point, ready to write.
//
// A point replaces the point already in its bucket, which is what makes the
// agent's at-least-once delivery safe: a redelivered payload writes the same
// bytes again. It is also why the intake writes the `.count` scalar first and
// stores sketches only for the buckets the TSDB accepted — the TSDB is
// append-only, so letting it rule on ordering keeps one decision, not two.
type keyValue struct {
	key, value []byte
}

// claim returns the id for ref, refusing one that another series already
// holds. newIDs carries the claims made earlier in the same batch, which are
// not in s.ids yet.
func (s *Store) claim(ref tsdb.SeriesRef, newIDs map[uint64]string) (uint64, error) {
	if err := ref.Validate(); err != nil {
		return 0, err
	}
	key := ref.Key()
	id := SeriesID(ref)
	held := s.known(id)
	if held == "" {
		held = newIDs[id]
	}
	if held != "" && held != key {
		s.collisions.Inc()
		s.log.Error("sketchstore: series id collision", "id", id, "held", held, "rejected", key)
		return 0, fmt.Errorf("%w: id %d is already %s", ErrIDCollision, id, held)
	}
	return id, nil
}

func (s *Store) known(id uint64) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ids[id]
}

// Read returns one series' sketches in [fromMs, toMs], ascending by time.
// Both bounds are inclusive, as they are for [tsdb.MetricStore.Select].
//
// It materializes the whole window, so it is for callers that know the window
// is small — tests, and anything holding a handful of buckets. The query path
// uses [Store.ReadEach], because a year of one series is three million
// sketches and this would hold all of them at once.
func (s *Store) Read(ctx context.Context, ref tsdb.SeriesRef, fromMs, toMs int64) ([]Point, error) {
	var out []Point
	err := s.ReadEach(ctx, ref, fromMs, toMs, func(p Point) error {
		out = append(out, p)
		return nil
	})
	return out, err
}

// ReadEach calls fn with each of one series' sketches in [fromMs, toMs],
// ascending by time, and stops early if fn returns an error.
//
// Streaming rather than returning a slice, because nothing bounds how many
// points a window holds. A query may cover a year, agents flush every ten
// seconds, and the query layer's own limit is on output *buckets*, not on
// stored points — so a single series can be three million sketches. Folding
// each one into its bucket as it arrives costs the buckets; collecting them
// first costs the window.
func (s *Store) ReadEach(ctx context.Context, ref tsdb.SeriesRef, fromMs, toMs int64, fn func(Point) error) (err error) {
	if toMs < fromMs {
		return nil
	}
	id := SeriesID(ref)
	if held := s.known(id); held != "" && held != ref.Key() {
		// Another series owns this id. Returning its sketches would be worse
		// than returning none.
		return nil
	}
	from := clampSeconds(fromMs)
	// The bound is exclusive, so it is the second after the last one wanted.
	to := clampSeconds(toMs)
	upper := seriesEnd(id)
	if to < MaxTimestamp {
		upper = pointKey(id, to+1)
	}
	it, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: pointKey(id, from),
		UpperBound: upper,
	})
	if err != nil {
		return fmt.Errorf("sketchstore: reading %s: %w", ref.Key(), err)
	}
	defer func() { err = errors.Join(err, it.Close()) }()

	for it.First(); it.Valid(); it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		k := it.Key()
		if len(k) != pointKeyLen {
			return fmt.Errorf("sketchstore: %s: a %d-byte point key", ref.Key(), len(k))
		}
		sk, err := decodeValue(it.Value())
		if err != nil {
			return fmt.Errorf("sketchstore: %s at %d: %w", ref.Key(), binary.BigEndian.Uint32(k[9:]), err)
		}
		if err := fn(Point{TimeMs: int64(binary.BigEndian.Uint32(k[9:])) * 1000, Sketch: sk}); err != nil {
			return err
		}
	}
	return it.Error()
}

// clampSeconds converts unix milliseconds to the seconds the key holds,
// keeping the result inside the 32-bit range the format allows.
func clampSeconds(ms int64) int64 {
	if ms < 0 {
		return 0
	}
	return min(ms/1000, MaxTimestamp)
}

// Truncate deletes every sketch older than beforeMs and forgets series left
// with nothing. It returns how many series were dropped entirely.
//
// One DeleteRange per series rather than one scan of everything: the key
// orders by series first and time second, so "older than t" is not a range —
// it is one range per series, and a store with a hundred thousand series
// issues a hundred thousand tiny tombstones rather than reading every point.
func (s *Store) Truncate(ctx context.Context, beforeMs int64) (droppedSeries int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if beforeMs <= 0 {
		return 0, nil
	}
	// Exclusive for the whole sweep: "this series is empty" is only a stable
	// answer while nothing is appending to it.
	s.sweepMu.Lock()
	defer s.sweepMu.Unlock()
	before := clampSeconds(beforeMs)
	s.mu.RLock()
	ids := make([]uint64, 0, len(s.ids))
	for id := range s.ids {
		ids = append(ids, id)
	}
	s.mu.RUnlock()
	// Sorted so the deletes go in key order, which is the order Pebble
	// prefers and the order a test can predict.
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	batch := s.db.NewBatch()
	defer func() { err = errors.Join(err, batch.Close()) }()
	var emptied []uint64
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if err := batch.DeleteRange(pointKey(id, 0), pointKey(id, before), nil); err != nil {
			return 0, fmt.Errorf("sketchstore: sweeping %d: %w", id, err)
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return 0, fmt.Errorf("sketchstore: committing the sweep: %w", err)
	}
	// Only now, with the deletes durable, is "has no points left" a question
	// with a stable answer.
	for _, id := range ids {
		empty, err := s.isEmpty(id)
		if err != nil {
			return 0, err
		}
		if empty {
			emptied = append(emptied, id)
		}
	}
	if len(emptied) == 0 {
		return 0, nil
	}
	drop := s.db.NewBatch()
	defer func() { err = errors.Join(err, drop.Close()) }()
	for _, id := range emptied {
		if err := drop.Delete(seriesKey(id), nil); err != nil {
			return 0, fmt.Errorf("sketchstore: forgetting %d: %w", id, err)
		}
	}
	if err := drop.Commit(pebble.Sync); err != nil {
		return 0, fmt.Errorf("sketchstore: forgetting %d series: %w", len(emptied), err)
	}
	s.mu.Lock()
	for _, id := range emptied {
		delete(s.ids, id)
	}
	s.mu.Unlock()
	return len(emptied), nil
}

func (s *Store) isEmpty(id uint64) (empty bool, err error) {
	it, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: pointKey(id, 0),
		UpperBound: seriesEnd(id),
	})
	if err != nil {
		return false, fmt.Errorf("sketchstore: checking %d: %w", id, err)
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	return !it.First(), it.Error()
}

// Sweep applies the retention window to now. It is what a maintenance loop
// calls; Truncate is the same thing with the cutoff spelled out.
func (s *Store) Sweep(ctx context.Context) (droppedSeries int, err error) {
	if s.retention < 0 {
		return 0, nil
	}
	return s.Truncate(ctx, s.clock.Now().Add(-s.retention-s.grace).UnixMilli())
}

// Stats reports the store's size.
func (s *Store) Stats() Stats {
	if s.closed.Load() {
		return Stats{}
	}
	s.mu.RLock()
	series := int64(len(s.ids))
	s.mu.RUnlock()
	var bytes int64
	if n, err := s.db.EstimateDiskUsage([]byte{0}, []byte{0xff}); err == nil {
		bytes = int64(n) //nolint:gosec // a size, and never near 2^63
	}
	return Stats{Series: series, DiskBytes: bytes}
}

// Close flushes and closes the database. Closing twice is a no-op: Pebble
// panics on a second close, and a shutdown path that closes a store twice is
// a bug worth surviving rather than a crash worth causing.
func (s *Store) Close() error {
	err := errors.New("sketchstore: already closed")
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		err = s.db.Close()
	})
	if err != nil && err.Error() == "sketchstore: already closed" {
		return nil
	}
	return err
}

// pebbleLogger sends Pebble's own chatter to the server's structured logger,
// so a compaction note does not arrive as an unattributed line on stderr.
type pebbleLogger struct{ log *slog.Logger }

func (l pebbleLogger) Infof(format string, args ...any) {
	l.log.Debug("pebble: " + fmt.Sprintf(format, args...))
}

func (l pebbleLogger) Errorf(format string, args ...any) {
	l.log.Error("pebble: " + fmt.Sprintf(format, args...))
}

// Fatalf must not return — Pebble calls it when it has found something it
// cannot continue past, such as a corrupt manifest. Panicking gives the
// process a stack and its deferred closes; returning would carry on with a
// database Pebble has just said is unusable.
func (l pebbleLogger) Fatalf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	l.log.Error("pebble: fatal", "err", msg)
	panic("sketchstore: pebble: " + msg)
}

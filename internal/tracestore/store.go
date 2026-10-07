package tracestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/klauspost/compress/zstd"

	"github.com/tuvo1106/ozymandias/internal/clock"
	"github.com/tuvo1106/ozymandias/internal/selfmetrics"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Defaults.
const (
	DefaultRetention  = 7 * 24 * time.Hour
	DefaultPendingTTL = 60 * time.Second
	// maxPending bounds the parked cross-service children. Past it the oldest are
	// dropped (and counted) rather than letting a stream of orphans grow memory.
	maxPending = 100_000
	// summaryResource is how much of a resource the entry index keeps. The full
	// text is in the span; the index only has to be enough to read a list by.
	summaryResource = 256
)

// Options configure a Store.
type Options struct {
	Dir string
	// Retention is how long a trace is kept; negative keeps everything, zero is the default.
	Retention time.Duration
	// PendingTTL is how long a cross-service child waits for its parent span to
	// arrive before the edge is given up on (counted, not fatal).
	PendingTTL time.Duration
	// NoSync skips the fsync on Append, for tests and benchmarks. A store with it
	// set must not be what acknowledges an intake request.
	NoSync   bool
	Clock    clock.Clock
	Logger   *slog.Logger
	Registry *selfmetrics.Registry
}

// Summary is what the entry index keeps about a top-level span: enough to list
// and filter without reading the span.
type Summary struct {
	Name       string `json:"name"`
	Resource   string `json:"resource"`
	Duration   int64  `json:"duration"`
	Error      int    `json:"error"`
	TraceError int    `json:"trace_error,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
}

// claim is one call's reservation of an entry key. ok is written before done is closed, so a
// waiter that has received from done may read it.
type claim struct {
	done chan struct{}
	ok   bool
}

// Store keeps spans in Pebble. Safe for concurrent use.
//
// The mental model is one primary table (every span, by trace id) and a few
// narrow indexes over the *entry* spans only. APM's questions are about
// requests (slow ones, failing ones, one route's), and an entry span is one per
// request per service, so the indexes are orders of magnitude smaller than the
// spans they point at. Everything else is derived: the service map from a
// counter updated as spans arrive, retention from a first-seen index.
type Store struct {
	db        *pebble.DB
	retention time.Duration
	pendTTL   time.Duration
	noSync    bool
	clock     clock.Clock
	log       *slog.Logger

	// sweepMu keeps retention away from in-flight appends: a sweep deletes a
	// trace's spans and then its index keys, and an append landing in between
	// would be left with index entries for spans that no longer exist.
	sweepMu sync.RWMutex

	enc *zstd.Encoder
	dec *zstd.Decoder

	mu      sync.Mutex
	known   map[string]struct{} // 'v' keys already written
	pending map[string][]pendingEdge
	npend   int
	// inflight holds the entry keys of Appends that have decided about their edges
	// but not yet committed, so two concurrent resends of one batch cannot both
	// count the same call.
	inflight map[string]*claim

	// failCommit, if set (tests only), is asked before each commit; a non-nil error is
	// returned instead of committing.
	failCommit func() error
	// afterReserve, if set (tests only), runs right after an entry key is reserved.
	afterReserve func()
	// afterDefer, if set (tests only), runs when a call skips past another call's reservation.
	afterDefer func()

	closed atomic.Bool

	spansAppended, entriesIndexed, edgesRecorded, pendingExpired, pendingDropped, sweptTraces *selfmetrics.Counter
}

type pendingEdge struct {
	env, child string
	hour       uint32
	err        bool
	dur        int64
	expires    time.Time
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
		opts.Retention = DefaultRetention
	}
	if opts.PendingTTL <= 0 {
		opts.PendingTTL = DefaultPendingTTL
	}
	db, err := pebble.Open(opts.Dir, &pebble.Options{Logger: pebbleLogger{opts.Logger}, Merger: edgeMerger})
	if err != nil {
		return nil, fmt.Errorf("tracestore: opening %s: %w", opts.Dir, err)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	r := opts.Registry
	s := &Store{
		db: db, retention: opts.Retention, pendTTL: opts.PendingTTL, noSync: opts.NoSync, clock: opts.Clock, log: opts.Logger,
		enc: enc, dec: dec, known: map[string]struct{}{}, pending: map[string][]pendingEdge{}, inflight: map[string]*claim{},
		spansAppended:  r.Counter("ozy.tracestore.spans_appended"),
		entriesIndexed: r.Counter("ozy.tracestore.entries_indexed"),
		edgesRecorded:  r.Counter("ozy.tracestore.edges_recorded"),
		pendingExpired: r.Counter("ozy.tracestore.edges_unresolved"),
		pendingDropped: r.Counter("ozy.tracestore.edges_dropped"),
		sweptTraces:    r.Counter("ozy.tracestore.traces_swept"),
	}
	if err := s.loadKnown(); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	r.GaugeFunc("ozy.tracestore.disk_bytes", func() float64 {
		if s.closed.Load() {
			return 0
		}
		return float64(s.db.Metrics().DiskSpaceUsage())
	})
	r.GaugeFunc("ozy.tracestore.edges_pending", func() float64 { s.mu.Lock(); defer s.mu.Unlock(); return float64(s.npend) })
	return s, nil
}

func (s *Store) loadKnown() error {
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{prefixService}, UpperBound: []byte{prefixService + 1}})
	if err != nil {
		return err
	}
	for ok := it.First(); ok; ok = it.Next() {
		s.known[string(it.Key())] = struct{}{}
	}
	return errors.Join(it.Error(), it.Close())
}

// Close closes the database. Operations after Close return an error.
func (s *Store) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	_ = s.enc.Close()
	s.dec.Close()
	return s.db.Close()
}

func (s *Store) writeOpts() *pebble.WriteOptions {
	if s.noSync {
		return pebble.NoSync
	}
	return pebble.Sync
}

// Append stores spans. It returns nil only once they are durable (fsynced, unless
// NoSync): ozyd's intake acknowledges the agent on that. Appending the same span
// twice is harmless: every key it writes is a pure function of the span, so the
// second write replaces the first with itself. The edge counters are the one
// write that is not, so an edge is counted only when its entry key is new: an
// agent that resends a batch whose acknowledgement was lost does not double
// the service map.
func (s *Store) Append(ctx context.Context, spans []wire.Span) error {
	if len(spans) == 0 {
		return nil
	}
	if s.closed.Load() {
		return errors.New("tracestore: closed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.sweepMu.RLock()
	defer s.sweepMu.RUnlock()

	b := s.db.NewBatch()
	defer func() { _ = b.Close() }()

	// What the batch says about each trace, before any span is written, so a
	// failing child marks the entry span of its own chunk.
	traceErr := map[string]bool{}
	byID := make(map[string]*wire.Span, len(spans))
	firstHour := map[string]uint32{}
	for i := range spans {
		sp := &spans[i]
		byID[sp.TraceID+sp.SpanID] = sp
		if sp.Error == 1 {
			traceErr[sp.TraceID] = true
		}
		if h := hourOf(sp.Start); firstHour[sp.TraceID] == 0 || h < firstHour[sp.TraceID] {
			firstHour[sp.TraceID] = h
		}
	}

	var newKnown []string
	own := map[string]*claim{} // entry keys this call reserved in s.inflight
	var deferred []*claim      // other calls' reservations this call saw and skipped past
	committed := false
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		s.mu.Lock()
		for k, c := range own {
			c.ok = committed
			close(c.done)
			delete(s.inflight, k)
		}
		s.mu.Unlock()
	}
	defer release()
	var taken []edgeDelta // pending edges removed from the waiting map; put back if the commit fails
	var takenKeys []string
	var edges []edgeDelta
	var parked []parkedEdge
	for i := range spans {
		sp := &spans[i]
		trace, _ := decodeID(sp.TraceID, traceLen)
		span, _ := decodeID(sp.SpanID, spanLen)
		if trace == nil || span == nil {
			continue // the wire decoder never lets this through; a direct caller might
		}
		raw, err := json.Marshal(sp)
		if err != nil {
			return fmt.Errorf("tracestore: encoding span: %w", err)
		}
		if err := b.Set(spanKey(trace, span), s.enc.EncodeAll(raw, nil), nil); err != nil {
			return err
		}
		if !sp.TopLevel() {
			continue
		}
		env := sp.Meta["env"]
		sum := Summary{Name: sp.Name, Resource: truncate(sp.Resource, summaryResource), Duration: sp.Duration, Error: sp.Error}
		if traceErr[sp.TraceID] {
			sum.TraceError = 1
		}
		fmt.Sscanf(sp.Meta["http.status_code"], "%d", &sum.StatusCode) //nolint:errcheck // absent or malformed leaves 0
		val, _ := json.Marshal(sum)
		ek := entryKey(env, sp.Service, sp.Start, trace, span)
		// Reserve first, then look: checking before reserving lets a concurrent resend
		// slip in between another call's commit and its release and count the call again.
		// A call that finds the key reserved by another call does not write it: only the
		// claimant publishes the key, together with its edge in one commit. Were a
		// duplicate to write it, its commit could land before the claimant's look (so
		// neither counts the edge), or the claimant's commit could fail after the
		// duplicate's succeeded (so the retry finds the key and skips the edge). The
		// duplicate then waits for the claimant before it reports success, and fails if
		// the claimant did, so that the agent resends. The claimant-fails case is pinned
		// by a test; the other is covered by the concurrent test only, which is
		// probabilistic.
		s.mu.Lock()
		c, reserved := s.inflight[string(ek)]
		_, mine := own[string(ek)]
		if !reserved {
			c = &claim{done: make(chan struct{})}
			s.inflight[string(ek)] = c
			own[string(ek)] = c
		}
		s.mu.Unlock()
		claimant := !reserved
		existed := reserved
		if reserved && !mine {
			deferred = append(deferred, c)
			if s.afterDefer != nil {
				s.afterDefer()
			}
		}
		if claimant {
			if s.afterReserve != nil {
				s.afterReserve()
			}
			existed = s.exists(ek)
			if err := b.Set(ek, val, nil); err != nil {
				return err
			}
		}
		if err := b.Set(resourceKey(env, sp.Service, sum.Resource, sp.Start, trace, span), nil, nil); err != nil {
			return err
		}
		if sp.Error == 1 || traceErr[sp.TraceID] {
			if err := b.Set(errorKey(env, sp.Service, sp.Start, trace, span), nil, nil); err != nil {
				return err
			}
		}
		s.mu.Lock()
		vk := string(serviceKey(env, sp.Service))
		_, seen := s.known[vk]
		s.mu.Unlock()
		if !seen {
			if err := b.Set([]byte(vk), nil, nil); err != nil {
				return err
			}
			newKnown = append(newKnown, vk)
		}
		if claimant {
			s.entriesIndexed.Inc()
		}

		if sp.ParentID != "" && !existed {
			d := edgeDelta{env: env, child: sp.Service, hour: hourOf(sp.Start), err: sp.Error == 1, dur: sp.Duration}
			if p, ok := byID[sp.TraceID+sp.ParentID]; ok {
				d.parent = p.Service
				edges = append(edges, d)
			} else if psvc, ok := s.parentService(trace, sp.ParentID); ok {
				d.parent = psvc
				edges = append(edges, d)
			} else {
				parked = append(parked, parkedEdge{key: sp.TraceID + sp.ParentID, edge: d})
			}
		}
	}
	for tid, h := range firstHour {
		trace, _ := decodeID(tid, traceLen)
		if err := b.Set(seenKey(h, trace), nil, nil); err != nil {
			return err
		}
	}
	// A span in this batch may be the parent somebody parked earlier.
	for i := range spans {
		key := spans[i].TraceID + spans[i].SpanID
		for _, d := range s.takePending(key) {
			taken = append(taken, d)
			takenKeys = append(takenKeys, key)
			d.parent = spans[i].Service
			edges = append(edges, d)
		}
	}
	if err := writeEdges(b, edges); err != nil {
		return err
	}
	var err error
	if s.failCommit != nil {
		err = s.failCommit()
	}
	if err == nil {
		err = b.Commit(s.writeOpts())
	}
	if err != nil {
		// The agent will resend the batch; the children it had been waiting on must
		// still be waiting when it does, or their edges are lost without a trace.
		parkedBack := make([]parkedEdge, len(taken))
		for i, d := range taken {
			parkedBack[i] = parkedEdge{key: takenKeys[i], edge: d}
		}
		s.park(parkedBack)
		return fmt.Errorf("tracestore: committing: %w", err)
	}
	committed = true
	release() // before waiting: two calls each waiting on the other's keys would otherwise deadlock
	for _, c := range deferred {
		select {
		case <-c.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if !c.ok {
			return errors.New("tracestore: a concurrent resend of this batch failed to commit; resend")
		}
	}
	s.mu.Lock()
	for _, k := range newKnown {
		s.known[k] = struct{}{}
	}
	s.mu.Unlock()
	s.park(parked)
	s.spansAppended.Add(int64(len(spans)))
	s.edgesRecorded.Add(int64(len(edges)))
	return nil
}

func (s *Store) exists(key []byte) bool {
	_, closer, err := s.db.Get(key)
	if err != nil {
		return false
	}
	_ = closer.Close()
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// Trace returns every stored span of a trace, oldest first, or nil if there are none.
func (s *Store) Trace(ctx context.Context, traceID string) ([]wire.Span, error) {
	trace, ok := decodeID(traceID, traceLen)
	if !ok {
		return nil, fmt.Errorf("tracestore: %q is not a trace id", traceID)
	}
	if s.closed.Load() {
		return nil, errors.New("tracestore: closed")
	}
	prefix := tracePrefix(trace)
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: prefix, UpperBound: successor(prefix)})
	if err != nil {
		return nil, err
	}
	var out []wire.Span
	for ok := it.First(); ok; ok = it.Next() {
		if err := ctx.Err(); err != nil {
			_ = it.Close()
			return nil, err
		}
		sp, err := s.decodeSpan(it.Value())
		if err != nil {
			s.log.Warn("tracestore: skipping an undecodable span", "trace", traceID, "err", err)
			continue
		}
		out = append(out, sp)
	}
	if err := errors.Join(it.Error(), it.Close()); err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(a, b wire.Span) int {
		switch {
		case a.Start != b.Start:
			return cmpInt(a.Start, b.Start)
		default:
			return cmpStr(a.SpanID, b.SpanID)
		}
	})
	return out, nil
}

func (s *Store) decodeSpan(v []byte) (wire.Span, error) {
	raw, err := s.dec.DecodeAll(v, nil)
	if err != nil {
		return wire.Span{}, err
	}
	var sp wire.Span
	return sp, json.Unmarshal(raw, &sp)
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// pebbleLogger sends Pebble's chatter to slog at debug.
type pebbleLogger struct{ l *slog.Logger }

func (p pebbleLogger) Infof(format string, args ...interface{}) {
	p.l.Debug(fmt.Sprintf(format, args...))
}
func (p pebbleLogger) Fatalf(format string, args ...interface{}) {
	p.l.Error(fmt.Sprintf(format, args...))
}

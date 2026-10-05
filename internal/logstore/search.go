package logstore

import (
	"container/heap"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/tuvo1106/ozymandias/internal/query/logql"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Limits and defaults for a search.
const (
	DefaultLimit = 100
	MaxLimit     = 10_000
	// DefaultScanBudget bounds the raw (decompressed) bytes one query may
	// decode before it stops and says so. It is what keeps an unselective
	// search over a week of logs from running until the client gives up.
	DefaultScanBudget = 2 << 30
)

// Order is the direction of a search.
type Order int

const (
	// Newest first, the default: a person looking at logs wants the latest.
	Newest Order = iota
	// Oldest first.
	Oldest
)

// SearchOpts tune a search. The zero value is the first page of 100, newest
// first, with the default scan budget.
type SearchOpts struct {
	Limit  int
	Order  Order
	Cursor string // from a previous SearchResult.Cursor; empty for the first page
	// ScanBudget is the raw bytes a query may decode; zero means DefaultScanBudget.
	ScanBudget int64
}

// SearchResult is a page of logs.
type SearchResult struct {
	Logs []wire.Log
	// Cursor continues the search after the last log returned; empty when
	// there is nothing more.
	Cursor string
	// Truncated means the scan budget ran out. What came back is still a
	// correct prefix in the requested order, and Cursor continues from it, but
	// logs further along were not examined.
	Truncated bool
	Stats     Stats
}

// Stats say what a query cost: what the index selected and what the scan read.
type Stats struct {
	Streams         int   // streams the index selected
	BlocksRead      int   // blocks decompressed
	BlocksSkipped   int   // blocks a bloom filter ruled out without decompressing them
	BytesRead       int64 // raw bytes of those blocks
	EntriesExamined int   // logs decoded and tested against the filter
}

// run is one sorted run of candidate entries: a head snapshot or a sealed
// block. Blocks are decoded lazily, only when the merge reaches their range.
type run struct {
	streamID int64
	pred     func(*wire.Log) bool
	minTs    int64
	maxTs    int64
	entries  []rawEntry // a head snapshot: already in memory, in (ts, seq) order
	path     string
	meta     BlockMeta
	isBlock  bool
	// need is what a block must contain for pred to match anything in it; a
	// bloom filter answers it without decompressing the block.
	need logql.Need
}

// snapshot selects streams with the plan's matchers and collects their runs
// overlapping [from, to], all under smu.RLock so that a concurrent seal is
// seen either entirely or not at all (see [Store]).
func (s *Store) snapshot(plan logql.Plan, from, to int64) ([]*run, int) {
	filters := make([]logql.Filter, len(plan.Branches))
	needs := make([]logql.Need, len(plan.Branches))
	for i, b := range plan.Branches {
		filters[i] = logql.Compile(b.Filter)
		needs[i] = logql.Needs(b.Filter)
	}
	s.smu.RLock()
	defer s.smu.RUnlock()

	var runs []*run
	selected := 0
	for _, st := range s.byID {
		var preds []logql.Filter
		var stNeeds []logql.Need
		for i, b := range plan.Branches {
			ok := true
			for _, m := range b.Matchers {
				if !m.Matches(st.labels.value(m.Key)) {
					ok = false
					break
				}
			}
			if ok {
				preds = append(preds, filters[i])
				stNeeds = append(stNeeds, needs[i])
			}
		}
		if len(preds) == 0 {
			continue
		}
		selected++
		pred := orFilters(preds)
		need := logql.OrNeeds(stNeeds)

		if len(st.head) > 0 {
			var es []rawEntry
			for _, e := range st.head {
				if e.Ts >= from && e.Ts <= to {
					es = append(es, e)
				}
			}
			if len(es) > 0 {
				sort.SliceStable(es, func(i, j int) bool { return less(es[i], es[j]) })
				runs = append(runs, &run{streamID: st.id, pred: pred, minTs: es[0].Ts, maxTs: es[len(es)-1].Ts, entries: es})
			}
		}
		for k, cs := range s.chunks {
			if k.stream != st.id {
				continue
			}
			for _, b := range cs.blocks {
				if b.MaxTs >= from && b.MinTs <= to {
					runs = append(runs, &run{streamID: st.id, pred: pred, minTs: b.MinTs, maxTs: b.MaxTs, path: cs.path, meta: b, isBlock: true, need: need})
				}
			}
		}
	}
	return runs, selected
}

func orFilters(fs []logql.Filter) func(*wire.Log) bool {
	if len(fs) == 1 {
		return fs[0]
	}
	return func(l *wire.Log) bool {
		for _, f := range fs {
			if f(l) {
				return true
			}
		}
		return false
	}
}

// less is the store's total order on entries: timestamp, then sequence number.
func less(a, b rawEntry) bool {
	if a.Ts != b.Ts {
		return a.Ts < b.Ts
	}
	return a.Seq < b.Seq
}

// ErrBadCursor is a cursor that did not come from this store's results (a
// caller error, not a storage one).
var ErrBadCursor = errors.New("logstore: malformed cursor")

type cursorKey struct {
	ts  int64
	seq uint64
}

func encodeCursor(e rawEntry) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(e.Ts, 10) + "." + strconv.FormatUint(e.Seq, 10)))
}

func decodeCursor(c string) (cursorKey, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return cursorKey{}, ErrBadCursor
	}
	ts, seq, ok := strings.Cut(string(raw), ".")
	t, err1 := strconv.ParseInt(ts, 10, 64)
	q, err2 := strconv.ParseUint(seq, 10, 64)
	if !ok || err1 != nil || err2 != nil {
		return cursorKey{}, ErrBadCursor
	}
	return cursorKey{t, q}, nil
}

// fileCache opens each chunk file once per query.
type fileCache map[string]*os.File

func (c fileCache) open(path string) (*os.File, error) {
	if f, ok := c[path]; ok {
		return f, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	c[path] = f
	return f, nil
}

func (c fileCache) close() {
	for _, f := range c {
		_ = f.Close()
	}
}

// ruledOut reports whether the run's block certainly holds no log matching its
// query, by its bloom filter. It reads only the filter (a few KiB), never the
// block. Anything doubtful (no filter, a filter that fails its checksum, a file
// that cannot be read) answers false: the block is then read, which is slower
// and always correct.
func (r *run) ruledOut(files fileCache) bool {
	if !r.isBlock || r.need.None() {
		return false
	}
	f, err := files.open(r.path)
	if err != nil {
		return false
	}
	b, ok := readBloom(f, r.meta)
	return ok && !b.mayMatch(r.need)
}

// load returns a run's entries. A block that has been deleted since the
// snapshot (retention) loads as nothing, not an error: the logs it held are
// gone, which is what retention means.
func (r *run) load(files fileCache) ([]rawEntry, error) {
	if !r.isBlock {
		return r.entries, nil
	}
	f, err := files.open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return readBlock(f, r.meta)
}

// Search returns the logs matching q in [from, to] (unix ms, both ends
// inclusive), newest first by default, a page at a time.
//
// The index selects streams, then a lazy merge reads only the blocks whose
// time range could still contribute to the next log in order: each block is
// decoded when the merge reaches its newest (or oldest) timestamp, so a
// newest-first search for 100 lines decodes a few blocks of the newest
// streams, not the week.
func (s *Store) Search(ctx context.Context, q logql.Node, from, to int64, opts SearchOpts) (*SearchResult, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	budget := opts.ScanBudget
	if budget <= 0 {
		budget = DefaultScanBudget
	}
	desc := opts.Order == Newest

	// The cursor narrows the range: everything strictly after it in the
	// search order. A boundary entry at the cursor's own timestamp is kept in
	// range and dropped by the key comparison below.
	var cur *cursorKey
	if opts.Cursor != "" {
		c, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, err
		}
		cur = &c
		if desc {
			to = min(to, c.ts)
		} else {
			from = max(from, c.ts)
		}
	}
	res := &SearchResult{}
	if from > to {
		return res, nil
	}
	runs, selected := s.snapshot(logql.Split(q), from, to)
	res.Stats.Streams = selected

	// Runs are decoded in the order their near edge is reached.
	if desc {
		sort.Slice(runs, func(i, j int) bool { return runs[i].maxTs > runs[j].maxTs })
	} else {
		sort.Slice(runs, func(i, j int) bool { return runs[i].minTs < runs[j].minTs })
	}
	files := fileCache{}
	defer files.close()

	h := &mergeHeap{desc: desc}
	next := 0
	var out []wire.Log
	var lastKey rawEntry
	// lastExamined is the last entry looked at, matching or not, and resume is where
	// a search stopped by the scan budget picks up if it has examined nothing yet.
	// A cursor that only knew the last *emitted* log could not move past a stretch of
	// non-matching blocks bigger than the budget: the search would stop there every time.
	var lastExamined rawEntry
	examined := false
	var resume *rawEntry
	more := false
scan:
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// Decode every run whose near edge is at or beyond the best entry in
		// hand: such a run might hold the next entry in order.
		for next < len(runs) && (h.Len() == 0 || runs[next].reaches(h.top().Ts, desc)) {
			r := runs[next]
			next++
			if r.ruledOut(files) {
				res.Stats.BlocksSkipped++
				continue
			}
			if r.isBlock {
				if budget < int64(r.meta.RawLen) && res.Stats.BlocksRead > 0 {
					// This block might hold an entry that sorts before
					// anything in hand, so nothing more may be emitted:
					// what has been is a correct prefix, and stopping here
					// keeps it one.
					res.Truncated = true
					if desc {
						resume = &rawEntry{Ts: r.maxTs + 1}
					} else {
						resume = &rawEntry{Ts: r.minTs - 1, Seq: math.MaxUint64}
					}
					break scan
				}
				budget -= int64(r.meta.RawLen)
				res.Stats.BlocksRead++
				res.Stats.BytesRead += int64(r.meta.RawLen)
			}
			es, err := r.load(files)
			if err != nil {
				return nil, err
			}
			if c := orderedFiltered(es, from, to, cur, desc); len(c) > 0 {
				heap.Push(h, &mergeCursor{run: r, es: c})
			}
		}
		if h.Len() == 0 {
			break
		}
		cr := h.cursors[0]
		e := cr.es[cr.i]
		cr.i++
		if cr.i == len(cr.es) {
			heap.Pop(h)
		} else {
			heap.Fix(h, 0)
		}
		res.Stats.EntriesExamined++
		lastExamined, examined = e, true
		l, err := decodeLog(e.Body)
		if err != nil {
			return nil, fmt.Errorf("logstore: decoding a stored log: %w", err)
		}
		if !cr.run.pred(l) {
			continue
		}
		if len(out) == limit {
			more = true // one more than asked for: there is a next page
			break
		}
		out = append(out, *l)
		lastKey = e
	}
	res.Logs = out
	switch {
	case more:
		res.Cursor = encodeCursor(lastKey)
	case res.Truncated && examined:
		res.Cursor = encodeCursor(lastExamined)
	case res.Truncated && resume != nil:
		res.Cursor = encodeCursor(*resume)
	}
	return res, nil
}

// reaches reports whether a run's near edge is at or beyond ts in the search
// direction, i.e. whether it could hold an entry that sorts at or before ts.
func (r *run) reaches(ts int64, desc bool) bool {
	if desc {
		return r.maxTs >= ts
	}
	return r.minTs <= ts
}

// orderedFiltered returns the entries of a run inside [from, to] and strictly
// after the cursor, in search order.
func orderedFiltered(es []rawEntry, from, to int64, cur *cursorKey, desc bool) []rawEntry {
	out := make([]rawEntry, 0, len(es))
	for _, e := range es {
		if e.Ts < from || e.Ts > to {
			continue
		}
		if cur != nil {
			k := cursorKey{e.Ts, e.Seq}
			// Keep only what sorts strictly after the cursor in the search order.
			after := k.ts > cur.ts || (k.ts == cur.ts && k.seq > cur.seq)
			before := k.ts < cur.ts || (k.ts == cur.ts && k.seq < cur.seq)
			if (desc && !before) || (!desc && !after) {
				continue
			}
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if desc {
			return less(out[j], out[i])
		}
		return less(out[i], out[j])
	})
	return out
}

type mergeCursor struct {
	run *run
	es  []rawEntry
	i   int
}

type mergeHeap struct {
	cursors []*mergeCursor
	desc    bool
}

func (h *mergeHeap) Len() int { return len(h.cursors) }
func (h *mergeHeap) Less(i, j int) bool {
	a, b := h.cursors[i].es[h.cursors[i].i], h.cursors[j].es[h.cursors[j].i]
	if h.desc {
		return less(b, a)
	}
	return less(a, b)
}
func (h *mergeHeap) Swap(i, j int) { h.cursors[i], h.cursors[j] = h.cursors[j], h.cursors[i] }
func (h *mergeHeap) Push(x any)    { h.cursors = append(h.cursors, x.(*mergeCursor)) }
func (h *mergeHeap) Pop() any {
	n := len(h.cursors)
	x := h.cursors[n-1]
	h.cursors = h.cursors[:n-1]
	return x
}
func (h *mergeHeap) top() rawEntry { c := h.cursors[0]; return c.es[c.i] }

package eval

import (
	"context"
	"fmt"
	"strings"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
)

// MaxQueriesPerBatch bounds one [Evaluator.Batch] call.
//
// A dashboard is the caller: twelve widgets of two or three queries is a dense
// one, and fifty leaves room for a table widget's columns without letting a
// single request stand in for an afternoon of them. The limit is on the *batch*
// rather than on the work, because the work is already bounded — one deadline
// and one series budget for the whole call, both below.
const MaxQueriesPerBatch = 50

// SelectionCacheBytes is how much one request may spend remembering selections
// so that later queries can reuse them.
//
// A cap rather than a count, because the thing being remembered varies by three
// orders of magnitude: one series of ten buckets is 80 bytes, and the largest a
// single node can produce is [MaxSeriesPerNode] x [MaxBuckets] x 8 = 12 MB. So
// "twenty entries" is a budget of anywhere between two kilobytes and 240
// megabytes, and a byte count is the only honest way to bound it.
//
// Sixteen megabytes: a little over one maximal selection, and room for dozens of
// realistic ones — a thousand series over a two-hour window at ten seconds is
// under a megabyte. It matters more than it looks, because this is *retained*
// memory, live until the request ends, and it is now paid by every query rather
// than only by a batch. Sixteen bounds a handful of concurrent requests at
// something a single-node ozyd can hold; sixty-four did not.
//
// When the budget runs out the request stops caching and keeps answering — a
// slow dashboard is better than a failed one, and the queries that already
// shared a selection keep their saving.
const SelectionCacheBytes = 16 << 20

// Outcome is one query's answer within a batch: a result, or the error that
// query failed with.
//
// Both rather than `([]Result, error)`, because a dashboard is a set of
// independent questions and one bad widget must not blank the other eleven. The
// caller decides what a failure is worth — the HTTP layer reports it per query,
// where it can be rendered in the widget that asked.
type Outcome struct {
	Result Result
	Err    error
}

// Batch evaluates several queries against one window, sharing what they can.
//
// # What is shared, and what is not
//
// The queries of a dashboard overlap heavily: `sum:http.request.count{service:api}
// by {route}` and `avg:http.request.count{service:api}` select the same thousand
// series and decode the same chunks to answer two different questions about them.
// Batch splits a query node in half — select-and-reduce-onto-the-grid, then
// group-and-aggregate — and memoizes the first half across the batch. The second
// half runs per query, because that is the part that differs.
//
// The cache key is the selector, the post-filter, the grid and the rollup method:
// everything the shared half depends on. It is deliberately not the query text,
// which would miss the case the sharing exists for — two different queries over
// one selection.
//
// This is also why the window and the interval belong to the batch rather than to
// each query. Two queries planned onto different grids share nothing, so
// per-query intervals would quietly turn a dashboard into N unrelated requests
// with extra steps; a dashboard has one time picker, so one grid is what it wants
// anyway (see ADR-0016).
//
// # One deadline for the batch
//
// [DefaultTimeout] bounds the whole call, not each query. Fifty queries of thirty
// seconds is twenty-five minutes, which is not a timeout, it is a promise nobody
// will wait for. A batch that runs out of time reports what it has and fails the
// rest with the deadline — again per query, so a dashboard draws the widgets that
// answered in time.
//
// Sequential on purpose: a cache that is filled concurrently needs locking, and
// the queries that would contend for a lock are exactly the ones that would have
// hit the cache. BenchmarkBatch_Shared against BenchmarkBatch_Separate is what
// the sharing is worth; ADR-0018 records the numbers.
func (e *Evaluator) Batch(ctx context.Context, reqs []Request) ([]Outcome, error) {
	if len(reqs) == 0 {
		return nil, badf("a batch needs at least one query")
	}
	if len(reqs) > MaxQueriesPerBatch {
		return nil, badf("a batch carries %d queries; the limit is %d", len(reqs), MaxQueriesPerBatch)
	}
	timeout := e.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	shared := &selections{budget: SelectionCacheBytes}
	out := make([]Outcome, len(reqs))
	for i, req := range reqs {
		res, err := e.evalWith(ctx, req, shared)
		out[i] = Outcome{Result: res, Err: err}
	}
	return out, nil
}

// evalWith is [Evaluator.Eval] without the timeout, against a given selection
// cache. Eval passes a cache of its own so that a single query behaves exactly
// as a batch of one — one code path, not two that can drift.
func (e *Evaluator) evalWith(ctx context.Context, req Request, shared *selections) (Result, error) {
	g, err := e.plan(req)
	if err != nil {
		return Result{}, err
	}
	st := &state{vars: req.Vars, shared: shared}
	f, err := e.node(ctx, req.Expr, g, st)
	if err != nil {
		return Result{}, err
	}
	res := Result{
		From:     req.From,
		To:       req.To,
		Interval: g.interval,
		Series:   f.lines(req.Expr, g),
		Warnings: st.warnings,
	}
	if res.Warnings == nil {
		res.Warnings = []string{}
	}
	return res, nil
}

// bucketed is one selected series, already reduced onto the grid: the unit of
// work two queries can share.
//
// The ref comes along because grouping needs the tags, and grouping is the half
// that is *not* shared — `by {route}` and `by {status}` both want this series and
// want different things from it.
type bucketed struct {
	ref    tsdb.SeriesRef
	values []float64
}

// selections memoizes select-and-reduce within one batch.
//
// Not safe for concurrent use, and not an LRU: it lives for one request, so
// nothing needs evicting, and when the budget is gone it simply stops taking
// entries. A cache that starts refusing work is worse than one that stops
// growing.
type selections struct {
	entries map[string][]bucketed
	budget  int
	// hits and stores are for the benchmark and the milestone notes: a sharing
	// claim nobody measured is a sharing claim.
	hits, stores int
}

// get returns a cached selection. A nil cache is a miss, which is what makes
// the cache optional at every call site.
func (s *selections) get(key string) ([]bucketed, bool) {
	if s == nil {
		return nil, false
	}
	series, ok := s.entries[key]
	if ok {
		s.hits++
	}
	return series, ok
}

// put remembers a selection if the budget allows.
func (s *selections) put(key string, series []bucketed) {
	if s == nil {
		return
	}
	size := 0
	for _, b := range series {
		// The values are the weight; the ref is a handful of short strings
		// beside them. Counted as the slice's own bytes rather than its cap,
		// because bucketize allocates exactly g.n floats.
		size += len(b.values) * 8
	}
	if size > s.budget {
		return
	}
	if s.entries == nil {
		s.entries = make(map[string][]bucketed)
	}
	s.budget -= size
	s.entries[key] = series
	s.stores++
}

// selectionKey identifies what a shared selection depends on: the selector the
// store sees, the matchers checked afterwards, the grid, and how samples reduce
// into a bucket. Two queries agreeing on all four are asking the store for the
// same thing.
//
// Every string is written with %q, which escapes, so no two different keys can
// spell themselves the same way — the failure mode of a hand-rolled cache key is
// a collision serving one query's data to another, and it is worth a few bytes to
// make that impossible rather than unlikely.
func selectionKey(sel tsdb.Selector, post postFilter, g grid, method rollup) string {
	var b strings.Builder
	fmt.Fprintf(&b, "m=%q", sel.Metric)
	for _, m := range sel.Matchers {
		fmt.Fprintf(&b, " s=%q/%d/%q", m.Key, m.Type, m.Value)
	}
	for _, m := range post {
		fmt.Fprintf(&b, " p=%q/%t/%t", m.Key, m.Neg, m.In)
		for _, v := range m.Values {
			fmt.Fprintf(&b, ",%q", v)
		}
	}
	fmt.Fprintf(&b, " g=%d/%d/%d r=%q", g.first, g.interval, g.n, method)
	return b.String()
}

// selectBucketed is the shared half of a query node: everything from the store
// up to but not including the grouping.
//
// Cached whole rather than per series. A partial selection is not a thing this
// can hand back — the [MaxSeriesPerNode] check is a property of the selection,
// not of any one series in it — so an entry is only written once the whole set
// has been walked without error. Which also means a cache hit can never be the
// selection that was refused for being too large: that one never got stored.
func (e *Evaluator) selectBucketed(
	ctx context.Context, q *metricql.Query, sel tsdb.Selector, post postFilter,
	g grid, method rollup, st *state,
) ([]bucketed, error) {
	key := selectionKey(sel, post, g, method)
	if series, ok := st.shared.get(key); ok {
		return series, nil
	}

	set, err := e.Store.Select(ctx, sel, g.first*1000, g.endMs()-1)
	if err != nil {
		return nil, err
	}
	defer set.Close()

	var series []bucketed
	selected := 0
	for set.Next() {
		ref := set.Series()
		if !post.matches(ref) {
			continue
		}
		if selected++; selected > MaxSeriesPerNode {
			return nil, tooManySeries(q)
		}
		values := bucketize(set.Iterator(), g, method)
		if values == nil {
			continue
		}
		// The ref is cloned because a store is entitled to reuse the tag slice
		// it handed out for the next series, and a cached entry outlives the
		// iterator it came from. Sharing a selection with a later query that
		// reads another query's tags is the kind of bug that looks like
		// corrupted data, not like a cache.
		series = append(series, bucketed{ref: cloneRef(ref), values: values})
	}
	if err := set.Err(); err != nil {
		return nil, err
	}
	st.shared.put(key, series)
	return series, nil
}

// cloneRef copies a ref's tags so it can outlive the iterator that produced it.
func cloneRef(ref tsdb.SeriesRef) tsdb.SeriesRef {
	tags := make([]tsdb.Tag, len(ref.Tags))
	copy(tags, ref.Tags)
	return tsdb.SeriesRef{Metric: ref.Metric, Tags: tags}
}

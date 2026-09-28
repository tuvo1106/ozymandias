package eval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// batch parses each query and evaluates them together, failing on a parse error
// — the batch's own failures are what a test is usually looking at.
func batch(t *testing.T, e *Evaluator, from, to, interval int64, queries ...string) []Outcome {
	t.Helper()
	reqs := make([]Request, len(queries))
	for i, q := range queries {
		n, err := metricql.Parse(q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		reqs[i] = Request{Expr: n, From: from, To: to, Interval: interval}
	}
	out, err := e.Batch(context.Background(), reqs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	return out
}

// The point of the endpoint: two questions about one selection ask the store
// once.
//
// Asserted on the store's own record rather than on the cache counters, because
// a cache that reports a hit while the store is asked anyway is the failure this
// is guarding against, and only one of those two can see it.
func TestBatch_TwoQueriesOverOneSelectionSelectOnce(t *testing.T) {
	e := fixture()
	store := e.Store.(*memStore)

	out := batch(t, e, 0, 60, 10,
		"sum:req.count{host:a} by {route}",
		"avg:req.count{host:a}",
		"max:req.count{host:a} by {host}",
	)
	for i, o := range out {
		if o.Err != nil {
			t.Fatalf("query %d: %v", i, o.Err)
		}
	}
	if len(store.selectors) != 1 {
		t.Errorf("the store was asked %d times for one selection: %v", len(store.selectors), store.selectors)
	}

	// And the answers are the ones each query would have got alone.
	for i, q := range []string{
		"sum:req.count{host:a} by {route}",
		"avg:req.count{host:a}",
		"max:req.count{host:a} by {host}",
	} {
		want := run(t, fixture(), q, 0, 60, 10)
		if got, wantLines := lines(out[i].Result), lines(want); !equalLines(got, wantLines) {
			t.Errorf("%s\n got: %v\nwant: %v", q, got, wantLines)
		}
	}
}

// Sharing must not reach across anything the selection depends on. Each of
// these differs from the first in one of the four parts of the key, so each
// costs its own trip to the store.
func TestBatch_DifferentSelectionsAreNotShared(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"a different metric", "sum:req.count{host:a}", "sum:temp{host:a}"},
		{"a different filter", "sum:req.count{host:a}", "sum:req.count{host:b}"},
		{"a different rollup", "sum:req.count{host:a}", "sum:req.count{host:a}.rollup(max)"},
		// A timeshift moves the window, so the grid differs even though the
		// selector does not.
		{"a different window", "sum:req.count{host:a}", "timeshift(sum:req.count{host:a}, 60)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := fixture()
			store := e.Store.(*memStore)
			out := batch(t, e, 0, 60, 10, tc.first, tc.second)
			for i, o := range out {
				if o.Err != nil {
					t.Fatalf("query %d: %v", i, o.Err)
				}
			}
			if len(store.selectors) != 2 {
				t.Errorf("%d selects, want 2 — a selection was shared that should not be: %v",
					len(store.selectors), store.selectors)
			}
		})
	}
}

// Every query in a batch answers exactly as it would alone. The sharing is an
// optimisation, and an optimisation that changes an answer is a bug, so this
// compares the two paths over the whole fixture rather than trusting one case.
func TestBatch_AgreesWithEvaluatingEachAlone(t *testing.T) {
	queries := []string{
		"sum:req.count{host:a} by {route}",
		"avg:req.count{host:a}",
		"sum:req.count{*} by {host}",
		"max:req.count{route:/x} by {host}",
		"min:req.count{route:/x}",
		"avg:temp{host:a}",
		"sum:cpu.rate{host:a}",
		"sum:req.count{host:a}.as_rate()",
		"sum:req.count{host:a} by {route} / sum:req.count{host:a}",
		"count:req.count{*} by {route}",
	}
	out := batch(t, fixture(), 0, 60, 10, queries...)
	for i, q := range queries {
		if out[i].Err != nil {
			t.Errorf("%s: %v", q, out[i].Err)
			continue
		}
		want := run(t, fixture(), q, 0, 60, 10)
		if got, wantLines := lines(out[i].Result), lines(want); !equalLines(got, wantLines) {
			t.Errorf("%s\n batch: %v\nalone: %v", q, got, wantLines)
		}
		if out[i].Result.Interval != want.Interval || out[i].Result.From != want.From {
			t.Errorf("%s: window %d/%d, want %d/%d", q,
				out[i].Result.From, out[i].Result.Interval, want.From, want.Interval)
		}
	}
}

// One bad widget must not blank the other eleven.
func TestBatch_OneFailedQueryDoesNotStopTheRest(t *testing.T) {
	out := batch(t, fixture(), 0, 60, 10,
		"sum:req.count{host:a}",
		"p95:temp{host:a}", // a percentile of a gauge: the query's own fault
		"avg:temp{host:a}",
	)
	if out[0].Err != nil || out[2].Err != nil {
		t.Errorf("a neighbour failed: %v / %v", out[0].Err, out[2].Err)
	}
	if out[1].Err == nil {
		t.Fatal("a percentile of a gauge was accepted")
	}
	if !errors.Is(out[1].Err, ErrBadQuery) {
		t.Errorf("%v is not classified as the query's fault", out[1].Err)
	}
	if len(out[0].Result.Series) == 0 || len(out[2].Result.Series) == 0 {
		t.Error("the queries that worked returned nothing")
	}
	// A failed query has no half-built result to render.
	if len(out[1].Result.Series) != 0 {
		t.Errorf("the failed query carries %d series", len(out[1].Result.Series))
	}
}

// A batch is bounded at both ends, and both messages are for whoever built it.
func TestBatch_RefusesNothingAndTooMuch(t *testing.T) {
	e := fixture()
	if _, err := e.Batch(context.Background(), nil); err == nil {
		t.Error("an empty batch was accepted")
	} else if !errors.Is(err, ErrBadQuery) {
		t.Errorf("%v is not the caller's fault", err)
	}

	n, err := metricql.Parse("sum:req.count{*}")
	if err != nil {
		t.Fatal(err)
	}
	reqs := make([]Request, MaxQueriesPerBatch+1)
	for i := range reqs {
		reqs[i] = Request{Expr: n, From: 0, To: 60, Interval: 10}
	}
	_, err = e.Batch(context.Background(), reqs)
	if err == nil {
		t.Fatalf("a batch of %d was accepted", len(reqs))
	}
	if !strings.Contains(err.Error(), fmt.Sprint(MaxQueriesPerBatch)) {
		t.Errorf("%v does not say what the limit is", err)
	}
	// The boundary itself is allowed.
	if _, err := e.Batch(context.Background(), reqs[:MaxQueriesPerBatch]); err != nil {
		t.Errorf("a batch of exactly %d was refused: %v", MaxQueriesPerBatch, err)
	}
}

// The budget is a cap on memory, not on correctness: when it is gone the batch
// stops sharing and keeps answering.
func TestBatch_AnExhaustedBudgetStillAnswers(t *testing.T) {
	e := fixture()
	store := e.Store.(*memStore)
	shared := &selections{budget: 0}

	queries := []string{"sum:req.count{host:a} by {route}", "avg:req.count{host:a}"}
	for i, q := range queries {
		n, err := metricql.Parse(q)
		if err != nil {
			t.Fatal(err)
		}
		res, err := e.evalWith(context.Background(), Request{Expr: n, From: 0, To: 60, Interval: 10}, shared)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		want := run(t, fixture(), q, 0, 60, 10)
		if got, wantLines := lines(res), lines(want); !equalLines(got, wantLines) {
			t.Errorf("query %d\n got: %v\nwant: %v", i, got, wantLines)
		}
	}
	if shared.stores != 0 {
		t.Errorf("%d entries were cached with no budget", shared.stores)
	}
	if len(store.selectors) != 2 {
		t.Errorf("%d selects, want 2 — nothing can be shared with no budget", len(store.selectors))
	}
}

// A selection is cached only once it has been walked to the end, so the
// selection that was refused for being too large cannot come back as a hit.
func TestBatch_ARefusedSelectionIsNotCached(t *testing.T) {
	var many []tsdb.SeriesSamples
	for i := 0; i <= MaxSeriesPerNode; i++ {
		many = append(many, series("wide", []string{fmt.Sprintf("id:%d", i)}, sec(0, 1)))
	}
	e := &Evaluator{Store: &memStore{series: many}, Types: types{"wide": wire.KindCount}, Timeout: -1}
	out := batch(t, e, 0, 60, 10, "sum:wide{*}", "avg:wide{*}")
	for i, o := range out {
		if o.Err == nil {
			t.Errorf("query %d selected %d series without complaint", i, len(many))
		}
	}
	// Both were refused on their own merits rather than the second being served
	// a cached half-selection.
	if !errors.Is(out[1].Err, ErrBadQuery) {
		t.Errorf("the second query failed with %v", out[1].Err)
	}
}

// The batch shares one deadline. Fifty queries of thirty seconds each is not a
// timeout; and a batch that runs out reports what it managed.
func TestBatch_OneDeadlineForTheWholeBatch(t *testing.T) {
	e := fixture()
	e.Timeout = 50 * time.Millisecond
	e.Store = &slowStore{MetricStore: e.Store, delay: 30 * time.Millisecond}

	// Each query selects a different metric so that nothing is shared and every
	// one of them pays the delay.
	reqs := []Request{}
	for _, q := range []string{"sum:temp{host:a}", "sum:req.count{host:a}", "sum:cpu.rate{host:a}"} {
		n, err := metricql.Parse(q)
		if err != nil {
			t.Fatal(err)
		}
		reqs = append(reqs, Request{Expr: n, From: 0, To: 60, Interval: 10})
	}
	out, err := e.Batch(context.Background(), reqs)
	if err != nil {
		t.Fatalf("the batch itself failed: %v", err)
	}
	if out[0].Err != nil {
		t.Errorf("the first query did not get its answer in time: %v", out[0].Err)
	}
	last := out[len(out)-1]
	if !errors.Is(last.Err, context.DeadlineExceeded) {
		t.Errorf("the last query failed with %v, want the deadline", last.Err)
	}
}

// slowStore makes every selection cost time, so a deadline can be reached
// without depending on how fast the machine running the test is.
type slowStore struct {
	tsdb.MetricStore
	delay time.Duration
}

func (s *slowStore) Select(ctx context.Context, sel tsdb.Selector, from, to int64) (tsdb.SeriesSet, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.MetricStore.Select(ctx, sel, from, to)
}

// A cache key is built from strings a user chose, so two different selections
// must not be able to spell one key: a collision serves one query's numbers to
// another, which looks like corrupted data rather than like a cache.
//
// %q escaping is what makes it impossible, so the cases below are the pairs that
// *would* collide without it — a metric name containing the separator the key
// uses between fields, and one tag value containing the comma the key uses
// between values. The first version of this test used values crafted the other
// way round, so it passed with the quoting removed; mutation caught that, which
// is the only reason this comment is accurate.
func TestSelectionKey_CannotCollide(t *testing.T) {
	g := grid{first: 0, interval: 10, n: 6}
	seen := map[string]string{}
	for _, tc := range []struct {
		name string
		sel  tsdb.Selector
		post postFilter
	}{
		{"plain", tsdb.Selector{Metric: "m", Matchers: []tsdb.Matcher{{Key: "a", Value: "b"}}}, nil},
		{"the quote is part of the value", tsdb.Selector{Metric: "m", Matchers: []tsdb.Matcher{{Key: "a", Value: `b" s="c`}}}, nil},
		{"two matchers", tsdb.Selector{Metric: "m", Matchers: []tsdb.Matcher{{Key: "a", Value: "b"}, {Key: "s", Value: "c"}}}, nil},
		// Spelled exactly as the key spells a metric plus one matcher.
		{"the metric carries the separator", tsdb.Selector{Metric: `m s=a/0/b`}, nil},
		{"the metric carries a quote", tsdb.Selector{Metric: `m" s="a`}, nil},
		{"a post-filter value", tsdb.Selector{Metric: "m"}, postFilter{{Key: "a", Values: []string{"b"}}}},
		{"the same value, negated", tsdb.Selector{Metric: "m"}, postFilter{{Key: "a", Neg: true, Values: []string{"b"}}}},
		{"two values", tsdb.Selector{Metric: "m"}, postFilter{{Key: "a", Values: []string{"b", "c"}}}},
		// Spelled exactly as the key spells two values.
		{"one value that looks like two", tsdb.Selector{Metric: "m"}, postFilter{{Key: "a", Values: []string{"b,c"}}}},
		{"one value carrying a quote", tsdb.Selector{Metric: "m"}, postFilter{{Key: "a", Values: []string{`b","c`}}}},
	} {
		key := selectionKey(tc.sel, tc.post, g, rollupAvg)
		if other, dup := seen[key]; dup {
			t.Errorf("%q and %q share the key %s", tc.name, other, key)
		}
		seen[key] = tc.name
	}
}

// equalLines compares rendered results.
func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// dashboard builds a store of nSeries series of one metric and the queries a
// widget grid would ask of them: several questions about one selection, which is
// the shape [Evaluator.Batch] exists for.
func dashboard(nSeries int) (*Evaluator, []Request) {
	var data []tsdb.SeriesSamples
	for i := 0; i < nSeries; i++ {
		samples := make([]tsdb.Sample, 0, 120)
		for t := 0; t < 120; t++ {
			samples = append(samples, tsdb.Sample{T: int64(t) * 10 * 1000, V: float64(i + t)})
		}
		data = append(data, series("req.count",
			[]string{fmt.Sprintf("route:/r%d", i%20), fmt.Sprintf("host:h%d", i/20), "service:api"}, samples))
	}
	e := &Evaluator{
		Store:   &memStore{series: data},
		Types:   types{"req.count": wire.KindCount},
		Timeout: -1,
	}
	var reqs []Request
	for _, q := range []string{
		"sum:req.count{service:api} by {route}",
		"avg:req.count{service:api} by {route}",
		"max:req.count{service:api} by {host}",
		"sum:req.count{service:api}",
		"count:req.count{service:api} by {route}",
		"sum:req.count{service:api} by {route}.as_rate()",
	} {
		n, err := metricql.Parse(q)
		if err != nil {
			panic(err)
		}
		reqs = append(reqs, Request{Expr: n, From: 0, To: 1200, Interval: 10})
	}
	return e, reqs
}

// BenchmarkBatch_Shared is the endpoint; BenchmarkBatch_Separate is the same six
// queries as six requests. The difference is what the sharing is worth: the
// numbers are recorded in ADR-0018, and again in docs/notes/M3.md when the
// milestone ends.
func BenchmarkBatch_Shared(b *testing.B) {
	e, reqs := dashboard(1000)
	ctx := context.Background()
	for b.Loop() {
		out, err := e.Batch(ctx, reqs)
		if err != nil {
			b.Fatal(err)
		}
		for _, o := range out {
			if o.Err != nil {
				b.Fatal(o.Err)
			}
		}
	}
}

func BenchmarkBatch_Separate(b *testing.B) {
	e, reqs := dashboard(1000)
	ctx := context.Background()
	for b.Loop() {
		for _, req := range reqs {
			if _, err := e.Eval(ctx, req); err != nil {
				b.Fatal(err)
			}
		}
	}
}

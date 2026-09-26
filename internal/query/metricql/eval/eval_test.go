package eval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/meta"
	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// memStore is a MetricStore over a fixed set of series, using the reference
// matcher semantics so that what the evaluator pushes down and what it filters
// afterwards agree with the real store.
type memStore struct {
	tsdb.MetricStore
	series []tsdb.SeriesSamples
	err    error
	// selectors records what was asked of the store, so a test can assert
	// that a filter was pushed down rather than only that the answer was
	// right.
	selectors []tsdb.Selector
}

func (m *memStore) Select(_ context.Context, sel tsdb.Selector, from, to int64) (tsdb.SeriesSet, error) {
	m.selectors = append(m.selectors, sel)
	if m.err != nil {
		return nil, m.err
	}
	var out []tsdb.SeriesSamples
	for _, s := range m.series {
		if !sel.Matches(s.Series) {
			continue
		}
		var smp []tsdb.Sample
		for _, x := range s.Samples {
			if x.T >= from && x.T <= to {
				smp = append(smp, x)
			}
		}
		if len(smp) > 0 {
			out = append(out, tsdb.SeriesSamples{Series: s.Series, Samples: smp})
		}
	}
	return tsdb.NewSliceSet(out), nil
}

// types is a MetricTypes over a map.
type types map[string]wire.Kind

func (t types) Metric(name string) (meta.Metric, bool) {
	k, ok := t[name]
	if !ok {
		return meta.Metric{}, false
	}
	return meta.Metric{Name: name, Type: k}, true
}

// sec builds samples from (unix seconds, value) pairs.
func sec(pairs ...float64) []tsdb.Sample {
	var out []tsdb.Sample
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, tsdb.Sample{T: int64(pairs[i]) * 1000, V: pairs[i+1]})
	}
	return out
}

func series(metric string, tags []string, samples []tsdb.Sample) tsdb.SeriesSamples {
	return tsdb.SeriesSamples{Series: tsdb.NewSeriesRef(metric, tags), Samples: samples}
}

// run parses and evaluates, failing the test on either.
func run(t *testing.T, e *Evaluator, q string, from, to, interval int64) Result {
	t.Helper()
	res, err := runErr(e, q, from, to, interval, nil)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return res
}

func runErr(e *Evaluator, q string, from, to, interval int64, vars map[string][]string) (Result, error) {
	n, err := metricql.Parse(q)
	if err != nil {
		return Result{}, err
	}
	return e.Eval(context.Background(), Request{Expr: n, From: from, To: to, Interval: interval, Vars: vars})
}

// lines renders a result as "scope: v,v,v" strings, which is compact enough to
// put a whole expected result on one line of a table.
func lines(res Result) []string {
	out := make([]string, 0, len(res.Series))
	for _, s := range res.Series {
		vals := make([]string, len(s.Points))
		for i, p := range s.Points {
			if math.IsNaN(p.V) {
				vals[i] = "_"
				continue
			}
			vals[i] = fmt.Sprintf("%g", p.V)
		}
		out = append(out, s.Scope()+": "+strings.Join(vals, ","))
	}
	return out
}

// The fixture: two routes on two hosts, at ten-second resolution over a
// sixty-second window, plus a gauge and a rate to exercise the type rules.
func fixture() *Evaluator {
	return &Evaluator{
		Store: &memStore{series: []tsdb.SeriesSamples{
			series("req.count", []string{"host:a", "route:/x"}, sec(0, 1, 10, 2, 20, 3, 30, 4, 40, 5, 50, 6)),
			series("req.count", []string{"host:b", "route:/x"}, sec(0, 10, 10, 20, 20, 30, 30, 40, 40, 50, 50, 60)),
			series("req.count", []string{"host:a", "route:/y"}, sec(0, 100, 30, 400)),
			series("temp", []string{"host:a"}, sec(0, 20, 10, 30, 30, 40, 40, 50)),
			series("cpu.rate", []string{"host:a"}, sec(0, 2, 10, 4)),
		}},
		Types: types{
			"req.count": wire.KindCount,
			"temp":      wire.KindGauge,
			"cpu.rate":  wire.KindRate,
			"lat":       wire.KindDistribution,
		},
		Timeout: -1,
	}
}

func TestEval_Pipeline(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		interval    int64
		want        []string
	}{
		{
			"a count sums across the series of a group",
			"sum:req.count{*}", 60,
			[]string{"*: 731"}, // 21 + 210 + 500, all in one bucket
		},
		{
			"grouping splits it",
			"sum:req.count{*} by {route}", 60,
			[]string{"route:/x: 231", "route:/y: 500"},
		},
		{
			"a count sums over time within a bucket, then across series",
			"sum:req.count{route:/x} by {route}", 30,
			[]string{"route:/x: 66,165"},
		},
		{
			"avg across series, after each series summed over time",
			"avg:req.count{route:/x} by {route}", 30,
			[]string{"route:/x: 33,82.5"},
		},
		{
			"max across series",
			"max:req.count{route:/x} by {route}", 30,
			[]string{"route:/x: 60,150"},
		},
		{
			"min across series",
			"min:req.count{route:/x} by {route}", 30,
			[]string{"route:/x: 6,15"},
		},
		{
			"count across series is how many reported",
			"count:req.count{*} by {route}", 60,
			[]string{"route:/x: 2", "route:/y: 1"},
		},
		{
			"a gauge averages over time rather than summing",
			"avg:temp{*}", 30,
			[]string{"*: 25,45"},
		},
		{
			"rollup overrides the metric's default",
			"avg:temp{*}.rollup(max)", 30,
			[]string{"*: 30,50"},
		},
		{
			"rollup(count) counts samples in the bucket",
			"avg:temp{*}.rollup(count)", 30,
			[]string{"*: 2,2"},
		},
		{
			"rollup(last) takes the newest sample",
			"avg:temp{*}.rollup(last)", 30,
			[]string{"*: 30,50"},
		},
		{
			"rollup(min)",
			"avg:temp{*}.rollup(min)", 30,
			[]string{"*: 20,40"},
		},
		{
			"an empty bucket is a gap, not a zero",
			"sum:req.count{route:/y} by {route}", 10,
			[]string{"route:/y: 100,_,_,400,_,_"},
		},
		{
			"fill(zero) makes it a zero",
			"sum:req.count{route:/y} by {route}.fill(zero)", 10,
			[]string{"route:/y: 100,0,0,400,0,0"},
		},
		{
			"fill(last) carries the previous value forward, never backward",
			"sum:req.count{route:/y} by {route}.fill(last)", 10,
			[]string{"route:/y: 100,100,100,400,400,400"},
		},
		{
			"fill(linear) interpolates between real points only",
			"sum:req.count{route:/y} by {route}.fill(linear)", 10,
			[]string{"route:/y: 100,200,300,400,_,_"},
		},
		{
			"fill's limit bounds how far it reaches",
			"sum:req.count{route:/y} by {route}.fill(last, 10)", 10,
			[]string{"route:/y: 100,100,_,400,400,_"},
		},
		{
			"as_rate divides by the bucket width",
			"sum:req.count{route:/x} by {route}.as_rate()", 30,
			[]string{"route:/x: 2.2,5.5"},
		},
		{
			"as_count on a count is the identity",
			"sum:req.count{route:/x} by {route}.as_count()", 30,
			[]string{"route:/x: 66,165"},
		},
		{
			"as_count on a rate multiplies by the width",
			"sum:cpu.rate{*}.as_count()", 30,
			[]string{"*: 180,_"},
		},
		{
			"a wildcard value",
			"sum:req.count{route:/*} by {route}", 60,
			[]string{"route:/x: 231", "route:/y: 500"},
		},
		{
			"a negated matcher excludes, and keeps series without the key",
			"sum:req.count{!host:a} by {host}", 60,
			[]string{"host:b: 210"},
		},
		{
			"an IN list is a disjunction",
			"sum:req.count{host IN (a,b)} by {host}", 60,
			[]string{"host:a: 521", "host:b: 210"},
		},
		{
			"a negated IN list",
			"sum:req.count{!host IN (b)} by {host}", 60,
			[]string{"host:a: 521"},
		},
		{
			"a series missing a group-by key groups under no value for it",
			"sum:req.count{*} by {nosuch}", 60,
			[]string{"*: 731"},
		},
		{
			"arithmetic between two queries joins by group",
			"sum:req.count{route:/x} by {route} / sum:req.count{*} by {route} * 100", 60,
			[]string{"route:/x: 100"},
		},
		{
			"a scalar broadcasts",
			"sum:req.count{route:/x} by {route} / 2", 60,
			[]string{"route:/x: 115.5"},
		},
		{
			"scalars fold without touching the store",
			"2 * 3 + 1", 60,
			[]string{"*: 7"},
		},
		{
			"negation",
			"-sum:req.count{route:/x} by {route}", 60,
			[]string{"route:/x: -231"},
		},
		{
			"division by zero is null, not infinity",
			"sum:req.count{route:/x} by {route} / (sum:req.count{route:/x} by {route} - sum:req.count{route:/x} by {route})", 60,
			[]string{"route:/x: _"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lines(run(t, fixture(), tc.query, 0, 59, tc.interval))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestEval_Errors(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		interval    int64
		contains    string
	}{
		{"a scalar agg on a distribution", "avg:lat{*}", 60, "is a distribution"},
		{"a percentile on a gauge", "p95:temp{*}", 60, "not a distribution"},
		{"a percentile with no sketch store", "p95:lat{*}", 60, "no sketch store"},
		{"as_rate on a gauge", "avg:temp{*}.as_rate()", 60, "does not apply"},
		{"as_count on a gauge", "avg:temp{*}.as_count()", 60, "does not apply"},
		{"conflicting rollup widths", "sum:req.count{*}.rollup(sum, 60) + sum:req.count{*}.rollup(sum, 30)", 0, "cannot be combined"},
		{"a rollup that fights the interval", "sum:req.count{*}.rollup(sum, 30)", 60, "cannot be combined"},
		{"an unbound variable", "sum:req.count{$env}", 60, "is not bound"},
		{"too many buckets", "sum:req.count{*}", 1, "the limit is"},
		{"an inverted window", "sum:req.count{*}", 60, "must be after"},
		{"a timeshift off the grid", "timeshift(sum:req.count{*}, -45)", 60, "whole number of"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from, to := int64(0), int64(59)
			if tc.name == "an inverted window" {
				from, to = 100, 50
			}
			if tc.name == "too many buckets" {
				to = 1_000_000
			}
			_, err := runErr(fixture(), tc.query, from, to, tc.interval, nil)
			if err == nil {
				t.Fatalf("%s succeeded", tc.query)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("error %q does not mention %q", err, tc.contains)
			}
		})
	}
}

// A join drops what it cannot match, and says so. Silence about a dropped line
// is how a dashboard comes to show four of its six services.
func TestEval_JoinDropsAndWarns(t *testing.T) {
	res, err := runErr(fixture(), "sum:req.count{*} by {route} - sum:req.count{route:/x} by {route}", 0, 59, 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := lines(res); len(got) != 1 || got[0] != "route:/x: 0" {
		t.Errorf("got %q, want only the matched group", got)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "route:/y") {
		t.Errorf("warnings %q, want one naming route:/y", res.Warnings)
	}
}

// Template variables are how a dashboard parameterises a query. An unbound one
// is an error (above); one bound to nothing is the "all" selection, which
// widens the query on purpose and says so.
func TestEval_TemplateVariables(t *testing.T) {
	res, err := runErr(fixture(), "sum:req.count{$scope} by {route}", 0, 59, 60,
		map[string][]string{"scope": {"route:/x"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := lines(res); len(got) != 1 || got[0] != "route:/x: 231" {
		t.Errorf("got %q, want only /x", got)
	}

	res, err = runErr(fixture(), "sum:req.count{$scope} by {route}", 0, 59, 60,
		map[string][]string{"scope": {}})
	if err != nil {
		t.Fatal(err)
	}
	if got := lines(res); len(got) != 2 {
		t.Errorf("got %q, want every route", got)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "$scope") {
		t.Errorf("warnings %q, want one about $scope", res.Warnings)
	}
}

// An equality filter must reach the store, or a metric with a million series
// is read in full to answer a question about three of them.
func TestEval_FiltersArePushedDown(t *testing.T) {
	e := fixture()
	store := e.Store.(*memStore)
	run(t, e, "sum:req.count{route:/x,!host:b} by {route}", 0, 59, 60)
	if len(store.selectors) != 1 {
		t.Fatalf("%d selects, want 1", len(store.selectors))
	}
	sel := store.selectors[0]
	if sel.Metric != "req.count" || len(sel.Matchers) != 2 {
		t.Fatalf("selector %+v did not carry both matchers", sel)
	}
	want := map[string]tsdb.MatchType{"route": tsdb.Equal, "host": tsdb.NotEqual}
	for _, m := range sel.Matchers {
		if want[m.Key] != m.Type {
			t.Errorf("matcher %+v: type %v, want %v", m, m.Type, want[m.Key])
		}
	}
}

// An IN list cannot be pushed down as a disjunction, so it narrows to "has the
// key" and the membership test runs in the evaluator. The store must still see
// the narrowing, or the fallback costs a full scan.
func TestEval_InNarrowsToKeyPresence(t *testing.T) {
	e := fixture()
	store := e.Store.(*memStore)
	run(t, e, "sum:req.count{host IN (a)} by {host}", 0, 59, 60)
	sel := store.selectors[0]
	if len(sel.Matchers) != 1 || sel.Matchers[0].Key != "host" || sel.Matchers[0].Type != tsdb.Wildcard {
		t.Errorf("selector %+v, want a wildcard on host", sel)
	}
}

func TestEval_SeriesLimit(t *testing.T) {
	var many []tsdb.SeriesSamples
	for i := range MaxSeriesPerNode + 1 {
		many = append(many, series("req.count", []string{fmt.Sprintf("host:h%04d", i)}, sec(0, 1)))
	}
	e := &Evaluator{Store: &memStore{series: many}, Types: types{"req.count": wire.KindCount}, Timeout: -1}
	_, err := runErr(e, "sum:req.count{*}", 0, 59, 60, nil)
	if err == nil || !strings.Contains(err.Error(), "narrow the filter") {
		t.Errorf("got %v, want the series limit to name its fix", err)
	}
}

func TestEval_StoreErrorsPropagate(t *testing.T) {
	boom := errors.New("store down")
	e := &Evaluator{Store: &memStore{err: boom}, Types: types{}, Timeout: -1}
	if _, err := runErr(e, "sum:req.count{*}", 0, 59, 60, nil); !errors.Is(err, boom) {
		t.Errorf("got %v, want the store's error", err)
	}
}

// The timeout is the query's, not the caller's: an expression that fans out
// over a thousand series must not be able to hold a connection open forever.
func TestEval_TimeoutApplies(t *testing.T) {
	e := fixture()
	e.Timeout = time.Nanosecond
	_, err := runErr(e, "sum:req.count{*}", 0, 59, 60, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want a deadline", err)
	}
}

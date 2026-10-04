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
		out = append(out, s.Scope+": "+strings.Join(vals, ","))
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
			"rollup(avg) overrides a count's default of summing",
			"sum:req.count{route:/x,host:a}.rollup(avg)", 30,
			[]string{"*: 2,5"},
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

// A dashboard's multi-select binds one variable to several values of one key
// and means *either* of them. Emitting two equality matchers would ask for a
// series tagged both, which no series is, and the chart would come back empty
// with nothing to explain why — the silent wrong answer, in the most ordinary
// case there is.
func TestEval_AMultiValuedVariableIsAChoice(t *testing.T) {
	for _, tc := range []struct {
		name string
		vars map[string][]string
		want []string
	}{
		{"one value", map[string][]string{"h": {"host:a"}}, []string{"host:a: 521"}},
		{
			"two values of one key are an OR",
			map[string][]string{"h": {"host:a", "host:b"}},
			[]string{"host:a: 521", "host:b: 210"},
		},
		{
			"two keys still AND",
			map[string][]string{"h": {"host:a", "route:/y"}},
			[]string{"host:a: 500"},
		},
		{
			"several values of one key, and another key",
			map[string][]string{"h": {"host:a", "host:b", "route:/x"}},
			[]string{"host:a: 21", "host:b: 210"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := runErr(fixture(), "sum:req.count{$h} by {host}", 0, 59, 60, tc.vars)
			if err != nil {
				t.Fatal(err)
			}
			if got := lines(res); strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The same intent written two ways has to mean the same thing, or one of the
// two spellings is a trap.
func TestEval_AVariableAndAnInListAgree(t *testing.T) {
	viaVar, err := runErr(fixture(), "sum:req.count{$h} by {host}", 0, 59, 60,
		map[string][]string{"h": {"host:a", "host:b"}})
	if err != nil {
		t.Fatal(err)
	}
	viaIn, err := runErr(fixture(), "sum:req.count{host IN (a,b)} by {host}", 0, 59, 60, nil)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := lines(viaVar), lines(viaIn); strings.Join(a, "|") != strings.Join(b, "|") {
		t.Errorf("a variable gave %q but the IN list gave %q", a, b)
	}
}

// fill's limit applies to every mode. Drawing a flat zero line forever after a
// series stopped reporting is the most misleading of the four: it looks like a
// measurement of nothing happening rather than an absence of measurement.
func TestEval_FillLimitAppliesToZeroToo(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  string
	}{
		{"sum:req.count{route:/y} by {route}.fill(zero)", "route:/y: 100,0,0,400,0,0"},
		{"sum:req.count{route:/y} by {route}.fill(zero, 10)", "route:/y: 100,0,_,400,0,_"},
		{"sum:req.count{route:/y} by {route}.fill(zero, 20)", "route:/y: 100,0,0,400,0,0"},
	} {
		got := lines(run(t, fixture(), tc.query, 0, 59, 10))
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.query, got, tc.want)
		}
	}
	// A leading run has no real data behind it to measure a limit from, so a
	// limited fill leaves it alone while an unlimited one takes it.
	e := fixture()
	e.Store.(*memStore).series = []tsdb.SeriesSamples{
		series("req.count", []string{"route:/z"}, sec(30, 7)),
	}
	for _, tc := range []struct {
		query string
		want  string
	}{
		{"sum:req.count{*} by {route}.fill(zero)", "route:/z: 0,0,0,7,0,0"},
		{"sum:req.count{*} by {route}.fill(zero, 10)", "route:/z: _,_,_,7,0,_"},
	} {
		got := lines(run(t, e, tc.query, 0, 59, 10))
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.query, got, tc.want)
		}
	}
}

// Modifiers run on the combined line, and fill is the reason. Filling per
// series would invent a reporting host: one that sent nothing would count as a
// zero in an avg: and drag it down by exactly as many hosts as were down.
func TestEval_FillRunsAfterTheSeriesAreCombined(t *testing.T) {
	e := fixture()
	e.Store.(*memStore).series = []tsdb.SeriesSamples{
		series("req.count", []string{"host:a"}, sec(0, 10, 30, 10)),
		series("req.count", []string{"host:b"}, sec(0, 20)), // stops reporting
	}
	// Bucket 1: only host:a reported, so the average of what reported is 10.
	// Filled per series it would be (10+0)/2 = 5 — a number about a host that
	// said nothing.
	got := lines(run(t, e, "avg:req.count{*}.fill(zero)", 0, 59, 30))
	if len(got) != 1 || got[0] != "*: 15,10" {
		t.Errorf("got %q, want *: 15,10", got)
	}
}

// The sentinel is for the API's status code, not for the reader. A message
// that begins "bad query: " tells whoever typed the query something they
// already know, and pushes the part they need past the fold.
func TestErrBadQuery_ClassifiesWithoutPrefixingTheMessage(t *testing.T) {
	err := badf("%s is a count, not a distribution", "x.count")
	if !errors.Is(err, ErrBadQuery) {
		t.Fatal("a refusal must classify as ErrBadQuery")
	}
	if got := err.Error(); got != "x.count is a count, not a distribution" {
		t.Errorf("message %q", got)
	}
	// And it survives being given more context on the way up, which is how
	// the evaluator names the node a refusal came from.
	wrapped := fmt.Errorf("evaluating %s: %w", "sum:x{*}", err)
	if !errors.Is(wrapped, ErrBadQuery) {
		t.Error("wrapping lost the classification")
	}
	if !strings.Contains(wrapped.Error(), "evaluating sum:x{*}: x.count is a count") {
		t.Errorf("wrapped message %q", wrapped)
	}
	// A store failure must not be mistaken for one.
	if errors.Is(errors.New("sql: database is closed"), ErrBadQuery) {
		t.Error("a store failure classified as a bad query")
	}
}

// fill(null) is the default written out: the gaps stay gaps. There is no
// fill applied by default (a "dashboard-level default" is mentioned in the
// grammar's comment but not implemented), so this pins only that the modifier is
// accepted and changes nothing; it cannot test an override, because nothing
// exists to override.
func TestEval_FillNullKeepsTheGaps(t *testing.T) {
	plain := lines(run(t, fixture(), "sum:req.count{route:/y} by {route}", 0, 59, 10))
	explicit := lines(run(t, fixture(), "sum:req.count{route:/y} by {route}.fill(null)", 0, 59, 10))
	if len(plain) != 1 || strings.Join(plain, "|") != strings.Join(explicit, "|") {
		t.Errorf("fill(null) drew %q, want the unfilled answer %q", explicit, plain)
	}
	if !strings.Contains(explicit[0], "_") {
		t.Errorf("%q has no gap in it, so the test is not testing a gap", explicit[0])
	}
}

// A selection that matches nothing is *no series*, not a series of zeros — and
// no modifier changes that, because fill works inside a series that exists.
//
// This is here because a dashboard depends on it. The obvious "5xx rate" widget
// is `narrow / wide * 100`, and for the healthy service the numerator matches
// nothing: the group is dropped, the widget draws an empty square, and an empty
// square reads as "broken" rather than as "zero". docs/dashboards.md tells
// template authors so, and the shipped service template draws its 5xx rate as a
// chart for this reason — so the claim needs to be a test rather than a
// sentence. If a modifier ever turns an empty selection into zero, this fails and
// both of those want rewriting. (The complement idiom below is not that: it
// moves the empty selection to the other side, and has its own limit.)
func TestEval_AnEmptySelectionIsNoSeriesRatherThanZero(t *testing.T) {
	for _, q := range []string{
		"sum:req.count{route:/nope}",
		"sum:req.count{route:/nope}.fill(zero)",
		"sum:req.count{route:/nope} by {route}.fill(zero)",
		"sum:req.count{route:/nope}.as_rate().fill(zero)",
	} {
		if got := lines(run(t, fixture(), q, 0, 59, 10)); len(got) != 0 {
			t.Errorf("%s drew %q, want nothing at all", q, got)
		}
	}
	// And so the ratio every error-rate widget wants to be is dropped, with a
	// warning that says which side had no match.
	res := run(t, fixture(), "sum:req.count{route:/nope} / sum:req.count{*} * 100", 0, 59, 10)
	if got := lines(res); len(got) != 0 {
		t.Fatalf("the ratio drew %q, want nothing", got)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "no match on the left") {
		t.Errorf("warnings %q do not say which side matched nothing", res.Warnings)
	}
	// The other way round is the same answer, so a template author cannot fix
	// it by swapping the operands.
	if got := lines(run(t, fixture(), "sum:req.count{*} / sum:req.count{route:/nope}", 0, 59, 10)); len(got) != 0 {
		t.Errorf("a missing denominator drew %q, want nothing", got)
	}
}

// "100 minus the share of everything that is NOT the thing" draws 0 for a
// healthy service without a new modifier, because the complement's selection
// exists whenever the thing is absent. It is NOT a general fix: when everything
// *is* the thing (every request a 5xx) the complement's selection is empty and
// the line vanishes, which is the one moment an error-rate chart must not go
// blank. That is why the shipped templates keep the direct ratio. Both halves
// are pinned here so that nobody takes the idiom for safe.
func TestEval_ComplementOfARatioIsZeroWhenNothingMatchesAndBlankWhenEverythingDoes(t *testing.T) {
	const total = "sum:req.count{*}"
	direct := run(t, fixture(), "sum:req.count{route:/y} / "+total+" * 100", 0, 59, 10)
	complement := run(t, fixture(), "100 - sum:req.count{!route:/y} / "+total+" * 100", 0, 59, 10)
	if len(direct.Series) != 1 || len(complement.Series) != 1 {
		t.Fatalf("direct %q, complement %q: want one line each", lines(direct), lines(complement))
	}
	compared := 0
	for i, p := range direct.Series[0].Points {
		if math.IsNaN(p.V) {
			continue
		}
		compared++
		if got := complement.Series[0].Points[i].V; math.Abs(got-p.V) > 1e-9 {
			t.Errorf("bucket %d: complement %g, direct %g", i, got, p.V)
		}
	}
	if compared == 0 {
		t.Fatal("no bucket had a direct answer, so nothing was compared")
	}

	// Nothing matches the thing: the complement is 0, in at least one real bucket.
	none := run(t, fixture(), "100 - sum:req.count{!route:/nope} / "+total+" * 100", 0, 59, 10)
	if len(none.Series) != 1 {
		t.Fatalf("complement drew %q, want one line of zeros", lines(none))
	}
	zeros := 0
	for i, p := range none.Series[0].Points {
		if math.IsNaN(p.V) {
			continue
		}
		if p.V != 0 {
			t.Errorf("bucket %d: %g, want 0 when nothing matched", i, p.V)
		}
		zeros++
	}
	if zeros == 0 {
		t.Error("the complement drew a line with no value in it, so 0 was never checked")
	}

	// Everything matches the thing: the complement's selection is empty, so
	// nothing is drawn, while the direct ratio draws 100.
	all := "sum:req.count{!route:/x,!route:/y} / " + total + " * 100"
	if got := lines(run(t, fixture(), "100 - "+all, 0, 59, 10)); len(got) != 0 {
		t.Errorf("when everything matches, the complement drew %q, want nothing (the limit this test documents)", got)
	}

	// No traffic at all is no answer either way.
	if got := lines(run(t, fixture(), "100 - sum:req.count{!route:/nope,host:zzz} / sum:req.count{host:zzz} * 100", 0, 59, 10)); len(got) != 0 {
		t.Errorf("with no traffic at all it drew %q, want nothing", got)
	}
}

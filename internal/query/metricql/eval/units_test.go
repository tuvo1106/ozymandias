package eval

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// A point is [ms, value] on the wire, and an empty bucket is null rather than
// zero — the one thing about this format a chart depends on.
func TestPoint_JSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Point
		want string
	}{
		{"a value", Point{T: 1000, V: 1.5}, `[1000,1.5]`},
		{"a gap", Point{T: 2000, V: math.NaN()}, `[2000,null]`},
		{"an infinity is a gap too", Point{T: 3000, V: math.Inf(1)}, `[3000,null]`},
		{"zero is a value, not a gap", Point{T: 4000, V: 0}, `[4000,0]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.p)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.want {
				t.Errorf("got %s, want %s", b, tc.want)
			}
			var back Point
			if err := json.Unmarshal(b, &back); err != nil {
				t.Fatal(err)
			}
			if back.T != tc.p.T || !sameValue(back.V, tc.p.V) && !math.IsInf(tc.p.V, 0) {
				t.Errorf("round trip gave %+v", back)
			}
		})
	}
	// A third element is ignored rather than refused: that is what unmarshalling
	// into a Go array does, and a Point is read from this server's own
	// responses, not from untrusted input.
	var p Point
	for _, bad := range []string{`[]`, `[null,1]`, `"x"`, `{}`} {
		if err := json.Unmarshal([]byte(bad), &p); err == nil {
			t.Errorf("Unmarshal(%s) succeeded", bad)
		}
	}
}

// The default interval aims for a chart's worth of points in whole multiples of
// the agent's flush, so a bucket is never narrower than the data in it.
func TestDefaultInterval(t *testing.T) {
	for _, tc := range []struct {
		from, to, want int64
	}{
		{0, 60, 10},     // a minute: the floor, not 0.2s
		{0, 3600, 20},   // an hour: 3600/300 = 12 -> 20
		{0, 86400, 290}, // a day
		{0, 7 * 86400, 2020},
	} {
		if got := DefaultInterval(tc.from, tc.to); got != tc.want {
			t.Errorf("DefaultInterval(%d, %d) = %d, want %d", tc.from, tc.to, got, tc.want)
		}
		if got := DefaultInterval(tc.from, tc.to); got%10 != 0 {
			t.Errorf("DefaultInterval(%d, %d) = %d, which is not a multiple of the flush", tc.from, tc.to, got)
		}
	}
}

// Functions whose answer depends on neighbouring buckets still have to accept a
// scalar, because a query may be `diff(5)` — a flat line, whose change is zero.
func TestEval_NeighbourFunctionsOfAScalar(t *testing.T) {
	for _, tc := range []struct{ query, want string }{
		{"diff(5)", "*: 0"},
		{"moving_avg(5, 3)", "*: 5"},
		{"top(5, 1)", "*: 5"},
	} {
		got := lines(run(t, fixture(), tc.query, 0, 59, 60))
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: got %q, want %q", tc.query, got, tc.want)
		}
	}
}

// as_rate on a metric nothing has recorded is allowed: there is no type to
// contradict, and the query returns nothing anyway. Refusing would make a
// dashboard fail for a service that has not started yet.
func TestEval_RateOnAnUnknownMetricIsAllowed(t *testing.T) {
	e := fixture()
	if _, err := runErr(e, "sum:never.seen{*}.as_rate()", 0, 59, 60, nil); err != nil {
		t.Errorf("got %v, want it allowed", err)
	}
}

func TestEval_ArithmeticErrorsPropagateFromEitherSide(t *testing.T) {
	e := fixture()
	for _, q := range []string{
		"avg:lat{*} + 1",
		"1 + avg:lat{*}",
		"abs(avg:lat{*})",
		"-avg:lat{*}",
		"top(avg:lat{*}, 1)",
	} {
		if _, err := runErr(e, q, 0, 59, 60, nil); err == nil {
			t.Errorf("%s: no error", q)
		}
	}
}

// interpolate's guards, which a query cannot reach through a well-formed
// histogram but a half-scraped one can.
func TestInterpolate_Guards(t *testing.T) {
	nan := math.NaN()
	for _, tc := range []struct {
		name   string
		bounds []float64
		counts []float64
		q      float64
		want   float64
	}{
		{"no observations", []float64{1, math.Inf(1)}, []float64{0, 0}, 0.5, nan},
		{"every bucket null", []float64{1, math.Inf(1)}, []float64{nan, nan}, 0.5, nan},
		{"a hole in the middle", []float64{1, 5, math.Inf(1)}, []float64{2, nan, 10}, 0.9, 1},
		{"the quantile is in the +Inf bucket", []float64{1, math.Inf(1)}, []float64{2, 10}, 0.9, 1},
		{"a first bucket at zero", []float64{0, math.Inf(1)}, []float64{5, 10}, 0.2, 0},
		{"a first bucket below zero", []float64{-5, math.Inf(1)}, []float64{5, 10}, 0.2, -5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := interpolate(tc.bounds, tc.counts, tc.q)
			if !sameValue(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseBound(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want float64
		ok   bool
	}{
		{"1", 1, true}, {"0.5", 0.5, true}, {"-10", -10, true},
		{"+Inf", math.Inf(1), true}, {"inf", math.Inf(1), true}, {"-Inf", math.Inf(-1), true},
		{"", 0, false}, {"abc", 0, false},
	} {
		got, err := parseBound(tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("parseBound(%q): err %v, want ok=%v", tc.in, err, tc.ok)
			continue
		}
		if tc.ok && got != tc.want {
			t.Errorf("parseBound(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// A grid's index rejects what is off it, which is how a store returning a
// sample just outside the window stays out of the answer.
func TestGrid_Index(t *testing.T) {
	g := grid{first: 100, interval: 10, n: 3}
	for _, tc := range []struct {
		sec int64
		i   int
		ok  bool
	}{
		{100, 0, true}, {109, 0, true}, {110, 1, true}, {129, 2, true},
		{99, 0, false}, {130, 0, false},
	} {
		i, ok := g.index(tc.sec)
		if ok != tc.ok || (ok && i != tc.i) {
			t.Errorf("index(%d) = (%d, %v), want (%d, %v)", tc.sec, i, ok, tc.i, tc.ok)
		}
	}
	if got := g.endMs(); got != 130_000 {
		t.Errorf("endMs = %d, want 130000", got)
	}
}

func TestFloorTo(t *testing.T) {
	for _, tc := range []struct{ t, w, want int64 }{
		{0, 10, 0}, {9, 10, 0}, {10, 10, 10}, {11, 10, 10},
		{-1, 10, -10}, {-10, 10, -10}, {-11, 10, -20},
	} {
		if got := floorTo(tc.t, tc.w); got != tc.want {
			t.Errorf("floorTo(%d, %d) = %d, want %d", tc.t, tc.w, got, tc.want)
		}
	}
}

// A metric with no recorded type reads as a gauge for time aggregation and is
// not a distribution, so it has no percentiles. Both matter: the first lets a
// query for an unwritten metric return nothing instead of failing, the second
// stops a percentile being answered from a store that holds no sketch for it.
func TestEval_UnknownMetricKind(t *testing.T) {
	e := &Evaluator{Store: &memStore{}, Timeout: -1}
	if k := e.metricKind("anything"); k != "" {
		t.Errorf("kind %q, want empty with no Types", k)
	}
	if got := defaultRollup(""); got != rollupAvg {
		t.Errorf("default rollup %q, want avg", got)
	}
	if got := defaultRollup(wire.KindCount); got != rollupSum {
		t.Errorf("count rollup %q, want sum", got)
	}
	e = fixture()
	if k := e.metricKind("never.seen"); k != "" {
		t.Errorf("kind %q for an unrecorded metric, want empty", k)
	}
}

func TestEval_NoQuery(t *testing.T) {
	e := fixture()
	if _, err := e.Eval(t.Context(), Request{From: 0, To: 59}); err == nil ||
		!strings.Contains(err.Error(), "no query") {
		t.Errorf("got %v, want a missing-query error", err)
	}
}

// A bare number is a line: a widget whose query is `100` draws a threshold
// across the whole window.
func TestEval_AScalarIsALine(t *testing.T) {
	res := run(t, fixture(), "100", 0, 59, 30)
	if len(res.Series) != 1 || len(res.Series[0].Points) != 2 {
		t.Fatalf("got %+v, want one line of two points", res.Series)
	}
	for _, p := range res.Series[0].Points {
		if p.V != 100 {
			t.Errorf("point %v, want 100 at every bucket", p)
		}
	}
	if res.Series[0].Scope() != "*" {
		t.Errorf("scope %q, want *", res.Series[0].Scope())
	}
}

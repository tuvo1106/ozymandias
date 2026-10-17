package spec

import (
	"strings"
	"testing"
	"time"
)

func TestParseQueryMetric(t *testing.T) {
	cases := []struct {
		in      string
		agg     TimeAgg
		window  time.Duration
		op      Op
		thr     float64
		grouped bool
		canon   string
	}{
		{"avg(last_5m):avg:jobs.queue.depth{service:shop-api} > 20", AggAvg, 5 * time.Minute, OpGT, 20, false,
			"avg(last_5m):avg:jobs.queue.depth{service:shop-api} > 20"},
		{"max(last_1h):p95:trace.http.request.duration{service:api} by {resource} >= 1.5", AggMax, time.Hour, OpGE, 1.5, true,
			"max(last_1h):p95:trace.http.request.duration{service:api} by {resource} >= 1.5"},
		{"  last(last_10m):sum:x{*} <= -3 ", AggLast, 10 * time.Minute, OpLE, -3, false, "last(last_10m):sum:x{*} <= -3"},
		{"pct_nonnull(last_3m):avg:heartbeat{*} < 50", AggPctNonNull, 3 * time.Minute, OpLT, 50, false, ""},
		{"sum(last_15m):sum:a{*} / sum:b{*} != 0", AggSum, 15 * time.Minute, OpNE, 0, false, ""},
		{"min(last_24h):avg:x{*} == 1e3", AggMin, 24 * time.Hour, OpEQ, 1000, false, ""},
		// A '>' inside a tag value is not the comparison: it is read from the right.
		{"avg(last_5m):avg:x{route:/a>b} > 1", AggAvg, 5 * time.Minute, OpGT, 1, false, ""},
	}
	for _, c := range cases {
		q, err := ParseQuery(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if q.Metric == nil || q.Log != nil {
			t.Fatalf("%q: not a metric query", c.in)
		}
		if q.Metric.Agg != c.agg || q.Window != c.window || q.Op != c.op || q.Threshold != c.thr {
			t.Errorf("%q: got %s %v %s %g", c.in, q.Metric.Agg, q.Window, q.Op, q.Threshold)
		}
		if q.Grouped() != c.grouped {
			t.Errorf("%q: grouped = %v", c.in, q.Grouped())
		}
		if c.canon != "" && q.String() != c.canon {
			t.Errorf("%q: canonical %q, want %q", c.in, q.String(), c.canon)
		}
		// Printing then parsing is the identity on what the evaluator reads.
		again, err := ParseQuery(q.String())
		if err != nil || again.String() != q.String() {
			t.Errorf("%q: round trip %q: %v", c.in, again.String(), err)
		}
	}
}

func TestParseQueryLog(t *testing.T) {
	q, err := ParseQuery(`logs("service:shop-worker status:error").rollup("count").last("5m") > 5`)
	if err != nil {
		t.Fatal(err)
	}
	if q.Log == nil || q.Log.Filter != "service:shop-worker status:error" || q.Log.By != "" ||
		q.Window != 5*time.Minute || q.Op != OpGT || q.Threshold != 5 || q.Grouped() {
		t.Fatalf("got %+v %+v", q, q.Log)
	}
	q, err = ParseQuery(`logs( "status:error \"disk full\"" ) .rollup("count") .by("service") .last("2h") >= 1`)
	if err != nil {
		t.Fatal(err)
	}
	if q.Log.Filter != `status:error "disk full"` || q.Log.By != "service" || q.Window != 2*time.Hour || !q.Grouped() {
		t.Fatalf("got %+v", q.Log)
	}
	if again, err := ParseQuery(q.String()); err != nil || again.String() != q.String() {
		t.Fatalf("round trip %q: %v", q.String(), err)
	}
	if _, err := ParseQuery(`logs("x").rollup("count").by("@http.status").last("5m") > 1`); err != nil {
		t.Fatalf("attribute group: %v", err)
	}
}

func TestParseQueryErrors(t *testing.T) {
	cases := map[string]string{
		"":                                                     "ends with a comparison",
		"avg(last_5m):avg:x{*}":                                "ends with a comparison",
		"avg(last_5m):avg:x{*} > ":                             "ends with a comparison",
		"avg(last_5m):avg:x{*} > abc":                          "ends with a comparison",
		"avg:x{*} > 1":                                         "starts with a time aggregation",
		"median(last_5m):avg:x{*} > 1":                         "time aggregation",
		"avg(last_5s):avg:x{*} > 1":                            "starts with a time aggregation",
		"avg(last_0m):avg:x{*} > 1":                            "window",
		"avg(last_25h):avg:x{*} > 1":                           "window",
		"avg(last_99999999999m):x{*} > 1":                      "window",
		"avg(last_5m): > 1":                                    "no metric query",
		"avg(last_5m):avg:x{ > 1":                              "metric query",
		`logs("x") > 1`:                                        "rollup",
		`logs("x").rollup("sum").last("5m") > 1`:               `only rollup is "count"`,
		`logs("x").rollup("count") > 1`:                        "last",
		`logs("x").rollup("count").last("5") > 1`:              "window",
		`logs("x").rollup("count").last("5m")x > 1`:            "unexpected",
		`logs("x).rollup("count").last("5m") > 1`:              "",
		`logs("x").rollup("count").by("a","b").last("5m") > 1`: "one field",
		`logs("x").rollup("count").by("color").last("5m") > 1`: "cannot group logs",
		`logs("status:").rollup("count").last("5m") > 1`:       "log query",
		"avg(last_5m):avg:x{*} > 1e999":                        "finite",
	}
	for in, want := range cases {
		_, err := ParseQuery(in)
		if err == nil {
			t.Errorf("%q: parsed", in)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %q, want it to mention %q", in, err, want)
		}
	}
	if _, err := ParseQuery(strings.Repeat("a", 5000)); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("long query: %v", err)
	}
}

func TestOpCompare(t *testing.T) {
	for _, c := range []struct {
		op   Op
		v, t float64
		want bool
	}{
		{OpGT, 2, 1, true}, {OpGT, 1, 1, false}, {OpGE, 1, 1, true}, {OpGE, 0, 1, false},
		{OpLT, 0, 1, true}, {OpLT, 1, 1, false}, {OpLE, 1, 1, true}, {OpLE, 2, 1, false},
		{OpEQ, 1, 1, true}, {OpEQ, 2, 1, false}, {OpNE, 2, 1, true}, {OpNE, 1, 1, false},
		{Op("~"), 1, 1, false},
	} {
		if got := c.op.Compare(c.v, c.t); got != c.want {
			t.Errorf("%g %s %g = %v", c.v, c.op, c.t, got)
		}
	}
}

// TestRollupMustTileTheGrid: an explicit rollup step is one the window and
// the store's epoch-aligned buckets can share.
func TestRollupMustTileTheGrid(t *testing.T) {
	for q, ok := range map[string]bool{
		"avg(last_5m):avg:x{*}.rollup(avg, 60) > 1":                            true,
		"avg(last_1h):sum:x{*}.rollup(sum, 3600) > 1":                          true,
		"avg(last_5m):avg:x{*}.rollup(avg, 3600) > 1":                          false, // no bucket at all
		"avg(last_5m):avg:x{*}.rollup(avg, 7) > 1":                             false, // does not divide a day
		"avg(last_5m):avg:x{*}.rollup(avg, 120) > 1":                           false, // 2.5 buckets
		"avg(last_5m):avg:x{*}.rollup(avg, 60) + avg:y{*}.rollup(avg, 30) > 1": false, // two grids
	} {
		_, err := ParseQuery(q)
		if (err == nil) != ok {
			t.Errorf("%s: err %v", q, err)
		}
	}
}

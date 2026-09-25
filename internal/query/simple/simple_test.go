package simple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// memStore is a MetricStore over a fixed set of series, with the reference
// matcher semantics.
type memStore struct {
	tsdb.MetricStore
	series []tsdb.SeriesSamples
	err    error
}

func (m *memStore) Select(_ context.Context, sel tsdb.Selector, from, to int64) (tsdb.SeriesSet, error) {
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

// render prints a result as "label: v v v" lines, "-" for null.
func render(r Result) string {
	var lines []string
	for _, s := range r.Series {
		vals := make([]string, len(s.Points))
		for i, p := range s.Points {
			if math.IsNaN(p.V) {
				vals[i] = "-"
			} else {
				vals[i] = fmt.Sprintf("%g", p.V)
			}
		}
		lines = append(lines, Label(s)+": "+strings.Join(vals, " "))
	}
	return strings.Join(lines, "\n")
}

var store = &memStore{series: []tsdb.SeriesSamples{
	series("req", []string{"route:/a", "env:dev"}, sec(100, 1, 110, 2, 120, 3, 130, 4)),
	series("req", []string{"route:/b", "env:dev"}, sec(100, 10, 120, 30)),
	series("req", []string{"route:/a", "env:prod"}, sec(100, 100, 110, 200)),
	series("req", []string{"env:dev"}, sec(110, 7)),
	series("lat", []string{"route:/a"}, sec(100, 1, 105, 3, 110, 5)),
}}

func run(t *testing.T, req Request) Result {
	t.Helper()
	r, err := Run(context.Background(), store, req)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRun_CountsSumOverTimeGaugesAverage(t *testing.T) {
	r := run(t, Request{Metric: "lat", From: 100, To: 119, Interval: 10, Kind: wire.KindGauge})
	if got := render(r); got != "lat{*}: 2 5" {
		t.Errorf("gauge: %s", got)
	}
	r = run(t, Request{Metric: "lat", From: 100, To: 119, Interval: 10, Kind: wire.KindCount})
	if got := render(r); got != "lat{*}: 4 5" {
		t.Errorf("count: %s", got)
	}
	r = run(t, Request{Metric: "lat", From: 100, To: 119, Interval: 20, Kind: wire.KindRate})
	if got := render(r); got != "lat{*}: 9" {
		t.Errorf("rate over 20s: %s", got)
	}
}

func TestRun_GroupByAndAggregators(t *testing.T) {
	base := Request{Metric: "req", From: 100, To: 139, Interval: 10, Kind: wire.KindCount}
	for _, tc := range []struct {
		agg  Agg
		by   []string
		want string
	}{
		{Sum, nil, "req{*}: 111 209 33 4"},
		{Avg, nil, "req{*}: 37 69.66666666666667 16.5 4"},
		{Min, nil, "req{*}: 1 2 3 4"},
		{Max, nil, "req{*}: 100 200 30 4"},
		{Sum, []string{"route"}, "req{*}: - 7 - -\nreq{route:/a}: 101 202 3 4\nreq{route:/b}: 10 - 30 -"},
		{Sum, []string{"env", "route"}, "req{env:dev,route:/a}: 1 2 3 4\nreq{env:dev,route:/b}: 10 - 30 -\nreq{env:dev}: - 7 - -\nreq{env:prod,route:/a}: 100 200 - -"},
	} {
		req := base
		req.Agg, req.By = tc.agg, tc.by
		if got := render(run(t, req)); got != tc.want {
			t.Errorf("%s by %v:\n got %s\nwant %s", tc.agg, tc.by, got, tc.want)
		}
	}
}

func TestRun_Filters(t *testing.T) {
	f, err := ParseFilter("env:dev,!route:/b")
	if err != nil {
		t.Fatal(err)
	}
	r := run(t, Request{Metric: "req", Filters: f, Agg: Sum, From: 100, To: 139, Interval: 10, Kind: wire.KindCount})
	if got := render(r); got != "req{*}: 1 9 3 4" {
		t.Errorf("got %s", got)
	}
}

func TestRun_EmptyBucketsAreNullAndTimestampsAreBucketStartsInMs(t *testing.T) {
	r := run(t, Request{Metric: "req", Filters: []tsdb.Matcher{{Key: "route", Value: "/b"}}, From: 95, To: 125, Interval: 10, Kind: wire.KindGauge})
	if len(r.Series) != 1 || len(r.Series[0].Points) != 4 {
		t.Fatalf("got %+v", r)
	}
	b, _ := json.Marshal(r.Series[0].Points)
	if string(b) != "[[90000,null],[100000,10],[110000,null],[120000,30]]" {
		t.Fatalf("points %s", b)
	}
	if r.From != 95 || r.To != 125 || r.Interval != 10 {
		t.Fatalf("echo %d %d %d", r.From, r.To, r.Interval)
	}
}

func TestRun_NoDataIsAnEmptyList(t *testing.T) {
	r := run(t, Request{Metric: "absent", From: 0, To: 100})
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"series":[]`) {
		t.Fatalf("got %s", b)
	}
}

func TestRun_StoreErrorIsReturned(t *testing.T) {
	_, err := Run(context.Background(), &memStore{err: errors.New("boom")}, Request{Metric: "m", From: 0, To: 10})
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v", err)
	}
}

// Samples the store hands back outside the query range don't make a line.
func TestBucketize_IgnoresOutOfRangeSamples(t *testing.T) {
	it := tsdb.NewSliceSet([]tsdb.SeriesSamples{{Samples: sec(0, 1, 500, 2)}})
	it.Next()
	if got := bucketize(it.Iterator(), 100, 10, 3, false); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestValidate(t *testing.T) {
	for name, tc := range map[string]struct {
		req  Request
		want string
	}{
		"bad metric":     {Request{Metric: "1x", From: 0, To: 10}, "valid metric name"},
		"empty range":    {Request{Metric: "m", From: 10, To: 10}, "must be after"},
		"bad agg":        {Request{Metric: "m", From: 0, To: 10, Agg: "p99"}, "agg"},
		"neg interval":   {Request{Metric: "m", From: 0, To: 10, Interval: -1}, "positive"},
		"bad by":         {Request{Metric: "m", From: 0, To: 10, By: []string{"a:b"}}, "not a tag key"},
		"too many":       {Request{Metric: "m", From: 0, To: 86400, Interval: 1}, "buckets"},
		"several errors": {Request{Metric: "", From: 5, To: 1, Agg: "x"}, "agg"},
		// The bucket count for these two wraps negative, so it slips past the
		// MaxBuckets check and Run allocates a slice of nonsense length.
		"from underflows": {Request{Metric: "m", From: math.MinInt64, To: 1790000000, Interval: 10}, "unix seconds"},
		"to overflows":    {Request{Metric: "m", From: 0, To: math.MaxInt64, Interval: 10}, "unix seconds"},
		// Few enough buckets, but 55 years of samples to fill them from.
		"range too long": {Request{Metric: "m", From: 0, To: 1790000000, Interval: 200000}, "query a shorter window"},
	} {
		err := tc.req.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	r := Request{Metric: "m", From: 0, To: 3600}
	if err := r.Validate(); err != nil || r.Agg != Avg || r.Interval != 20 {
		t.Fatalf("defaults: %v %+v", err, r)
	}
}

// TestRunRejectsUnboundedRange is the regression test for the panic: Run must
// reject an out-of-domain range rather than reach bucketize, whose
// make([]float64, n) panics outright on a wrapped bucket count.
func TestRunRejectsUnboundedRange(t *testing.T) {
	for _, req := range []Request{
		{Metric: "req", From: math.MinInt64, To: 1790000000, Interval: 10},
		{Metric: "req", From: 0, To: math.MaxInt64, Interval: 10},
		{Metric: "req", From: 0, To: 1790000000, Interval: 200000},
	} {
		if _, err := Run(context.Background(), store, req); err == nil {
			t.Errorf("Run(%+v) = nil error, want a rejection", req)
		}
	}
}

func TestDefaultInterval(t *testing.T) {
	for _, tc := range [][3]int64{{0, 60, 10}, {0, 3600, 20}, {0, 86400, 290}, {0, 3000, 10}, {0, 3001, 20}} {
		if got := DefaultInterval(tc[0], tc[1]); got != tc[2] {
			t.Errorf("DefaultInterval(%d,%d) = %d, want %d", tc[0], tc[1], got, tc[2])
		}
	}
}

func TestParseFilter(t *testing.T) {
	got, err := ParseFilter(" env:dev , !route:/b,route:/api/*,!host:web-*,canary,,url:http://x:1")
	if err != nil {
		t.Fatal(err)
	}
	want := []tsdb.Matcher{
		{Key: "env", Value: "dev", Type: tsdb.Equal},
		{Key: "route", Value: "/b", Type: tsdb.NotEqual},
		{Key: "route", Value: "/api/*", Type: tsdb.Wildcard},
		{Key: "host", Value: "web-*", Type: tsdb.NotWildcard},
		{Key: "canary", Value: "", Type: tsdb.Equal},
		{Key: "url", Value: "http://x:1", Type: tsdb.Equal},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	for _, bad := range []string{":x", "!:x"} {
		if _, err := ParseFilter(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if m, _ := ParseFilter(""); m != nil {
		t.Errorf("empty filter = %v", m)
	}
}

func TestPoint_JSON(t *testing.T) {
	for _, p := range []Point{{T: 1000, V: 1.5}, {T: 2000, V: math.NaN()}} {
		b, _ := json.Marshal(p)
		var back Point
		if err := json.Unmarshal(b, &back); err != nil || back.T != p.T || (back.V != p.V && !math.IsNaN(p.V)) {
			t.Errorf("%v → %s → %v (%v)", p, b, back, err)
		}
	}
	var p Point
	if json.Unmarshal([]byte(`[null,1]`), &p) == nil || json.Unmarshal([]byte(`{}`), &p) == nil {
		t.Error("bad point accepted")
	}
}

func TestLabel(t *testing.T) {
	if got := Label(Series{Metric: "m", Tags: map[string]string{"b": "2", "a": "", "c": "x:y"}}); got != "m{a,b:2,c:x:y}" {
		t.Fatal(got)
	}
}

// L2: summing everything at any interval gives the same grand total as the
// raw samples (for a count), and group-by + sum re-adds to the ungrouped sum.
func TestRun_SumIsConservedAcrossIntervalsAndGroupings(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		st := &memStore{}
		var total float64
		for i := range rapid.IntRange(1, 6).Draw(t, "series") {
			tags := []string{fmt.Sprintf("s:%d", i), "g:" + rapid.SampledFrom([]string{"x", "y"}).Draw(t, "g")}
			var smp []tsdb.Sample
			for _, ts := range rapid.SliceOfNDistinct(rapid.Int64Range(0, 999), 1, 20, func(v int64) int64 { return v }).Draw(t, "ts") {
				v := float64(rapid.IntRange(0, 100).Draw(t, "v"))
				smp = append(smp, tsdb.Sample{T: ts * 1000, V: v})
				total += v
			}
			sortSamples(smp)
			st.series = append(st.series, series("m", tags, smp))
		}
		iv := rapid.Int64Range(1, 400).Draw(t, "interval")
		for _, by := range [][]string{nil, {"g"}, {"s"}} {
			r, err := Run(context.Background(), st, Request{Metric: "m", Agg: Sum, By: by, From: 0, To: 999, Interval: iv, Kind: wire.KindCount})
			if err != nil {
				t.Fatal(err)
			}
			var got float64
			for _, s := range r.Series {
				for _, p := range s.Points {
					if !math.IsNaN(p.V) {
						got += p.V
					}
				}
			}
			if got != total {
				t.Fatalf("interval %d by %v: Σ %v, want %v", iv, by, got, total)
			}
		}
	})
}

func sortSamples(s []tsdb.Sample) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].T < s[j-1].T; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func TestFloorTo(t *testing.T) {
	if floorTo(-5, 10) != -10 || floorTo(15, 10) != 10 {
		t.Fatal("floorTo")
	}
}

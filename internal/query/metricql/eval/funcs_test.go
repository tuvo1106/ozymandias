package eval

import (
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

func TestEval_Functions(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		interval    int64
		want        []string
	}{
		{"abs", "abs(-sum:req.count{route:/x} by {route})", 60, []string{"route:/x: 231"}},
		{"log2", "log2(sum:req.count{route:/y} by {route})", 30, []string{"route:/y: 6.643856189774724,8.643856189774725"}},
		{"log10", "log10(sum:req.count{route:/y} by {route})", 30, []string{"route:/y: 2,2.602059991327962"}},
		{
			// log of zero is not a number, and -Inf would rescale a chart to
			// nothing, so it is a gap.
			"log of zero is null",
			"log10(sum:req.count{route:/y} by {route}.fill(zero))", 10,
			[]string{"route:/y: 2,_,_,2.602059991327962,_,_"},
		},
		{"clamp_min", "clamp_min(sum:req.count{route:/x} by {route}, 300)", 30, []string{"route:/x: 300,300"}},
		{"clamp_max", "clamp_max(sum:req.count{route:/x} by {route}, 100)", 30, []string{"route:/x: 66,100"}},
		{"clamp_min with a negative bound", "clamp_min(-sum:req.count{route:/x} by {route}, -100)", 30, []string{"route:/x: -66,-100"}},
		{
			// The first bucket has no predecessor, so it has no increment.
			"diff",
			"diff(sum:req.count{route:/x} by {route})", 30,
			[]string{"route:/x: _,99"},
		},
		{
			"diff across a gap is null on both sides of it",
			"diff(sum:req.count{route:/y} by {route})", 10,
			[]string{"route:/y: _,_,_,_,_,_"},
		},
		{
			"moving_avg over two buckets",
			"moving_avg(sum:req.count{route:/x} by {route}, 2)", 30,
			[]string{"route:/x: 66,115.5"},
		},
		{
			"moving_avg skips nulls rather than counting them as zero",
			"moving_avg(sum:req.count{route:/y} by {route}, 2)", 10,
			[]string{"route:/y: 100,100,_,400,400,_"},
		},
		{"nested calls", "clamp_min(abs(-sum:req.count{route:/x} by {route}), 0)", 60, []string{"route:/x: 231"}},
		{"a function of a scalar", "abs(-5)", 60, []string{"*: 5"}},
		{
			"top keeps the busiest lines",
			`top(sum:req.count{*} by {host,route}, 1, "sum", "desc")`, 60,
			[]string{"host:a,route:/y: 500"},
		},
		{
			"top ascending keeps the quietest",
			`top(sum:req.count{*} by {host,route}, 1, "sum", "asc")`, 60,
			[]string{"host:a,route:/x: 21"},
		},
		{
			"top defaults to the mean, descending",
			"top(sum:req.count{*} by {host,route}, 2)", 60,
			[]string{"host:a,route:/y: 500", "host:b,route:/x: 210"},
		},
		{
			"top by last",
			`top(sum:req.count{*} by {host,route}, 1, "last", "desc")`, 30,
			[]string{"host:a,route:/y: 100,400"},
		},
		{
			"top by max",
			`top(sum:req.count{*} by {host,route}, 1, "max", "desc")`, 30,
			[]string{"host:a,route:/y: 100,400"},
		},
		{
			"top by min",
			`top(sum:req.count{*} by {host,route}, 1, "min", "asc")`, 30,
			[]string{"host:a,route:/x: 6,15"},
		},
		{
			"top of more lines than there are keeps them all",
			"top(sum:req.count{*} by {route}, 10)", 60,
			[]string{"route:/x: 231", "route:/y: 500"},
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

// histogram_quantile is the path for a metric scraped from an exporter, where
// cumulative buckets are all there is. The numbers here are hand-checked
// against the linear interpolation Prometheus does, because "it matches the
// code" would be no test at all.
func TestEval_HistogramQuantile(t *testing.T) {
	// One histogram: 10 observations, cumulative — 2 at or below 1, 6 at or
	// below 5, 10 at or below 10, 10 in total.
	e := &Evaluator{
		Store: &memStore{series: []tsdb.SeriesSamples{
			series("lat.bucket", []string{"upper_bound:1"}, sec(0, 2)),
			series("lat.bucket", []string{"upper_bound:5"}, sec(0, 6)),
			series("lat.bucket", []string{"upper_bound:10"}, sec(0, 10)),
			series("lat.bucket", []string{"upper_bound:+Inf"}, sec(0, 10)),
		}},
		Types:   types{"lat.bucket": wire.KindGauge},
		Timeout: -1,
	}
	for _, tc := range []struct {
		q    string
		want string
	}{
		// rank 5 of 10 falls in (1,5]: 1 + (5-1)*(5-2)/(6-2) = 4
		{"0.5", "*: 4"},
		// rank 9 falls in (5,10]: 5 + (10-5)*(9-6)/(10-6) = 8.75
		{"0.9", "*: 8.75"},
		// rank 2 is exactly the top of the first bucket
		{"0.2", "*: 1"},
		// the maximum is the largest finite boundary; +Inf is not an answer
		{"1", "*: 10"},
		// q=0 interpolates from the assumed zero floor of the first bucket,
		// which is what Prometheus does
		{"0", "*: 0"},
	} {
		t.Run("q="+tc.q, func(t *testing.T) {
			q := "histogram_quantile(" + tc.q + ", avg:lat.bucket{*} by {upper_bound})"
			got := lines(run(t, e, q, 0, 59, 60))
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEval_HistogramQuantileErrors(t *testing.T) {
	e := fixture()
	for _, tc := range []struct{ name, query, contains string }{
		{
			"without the boundary in the grouping",
			"histogram_quantile(0.9, sum:req.count{*})",
			"add `by {upper_bound}`",
		},
		{
			"of a number",
			"histogram_quantile(0.9, 3)",
			"not a number",
		},
		{
			"outside [0,1]",
			"histogram_quantile(2, sum:req.count{*} by {upper_bound})",
			"quantile in [0,1]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runErr(e, tc.query, 0, 59, 60, nil)
			if err == nil || !strings.Contains(err.Error(), tc.contains) {
				t.Errorf("got %v, want it to mention %q", err, tc.contains)
			}
		})
	}
}

// A boundary that is not a number is corruption from a scrape, not a reason to
// answer anyway.
func TestEval_HistogramQuantileRejectsABadBoundary(t *testing.T) {
	e := &Evaluator{
		Store: &memStore{series: []tsdb.SeriesSamples{
			series("h", []string{"upper_bound:wat"}, sec(0, 1)),
		}},
		Types:   types{"h": wire.KindGauge},
		Timeout: -1,
	}
	_, err := runErr(e, "histogram_quantile(0.5, avg:h{*} by {upper_bound})", 0, 59, 60, nil)
	if err == nil || !strings.Contains(err.Error(), "not a bucket boundary") {
		t.Errorf("got %v, want a boundary error", err)
	}
}

// A histogram of a signed quantity has negative bounds, and +Inf is the last
// bucket of every Prometheus histogram.
func TestEval_HistogramQuantileNegativeBounds(t *testing.T) {
	e := &Evaluator{
		Store: &memStore{series: []tsdb.SeriesSamples{
			series("h", []string{"upper_bound:-10"}, sec(0, 1)),
			series("h", []string{"upper_bound:0"}, sec(0, 2)),
			series("h", []string{"upper_bound:+Inf"}, sec(0, 4)),
		}},
		Types:   types{"h": wire.KindGauge},
		Timeout: -1,
	}
	// rank 2 of 4 is exactly the top of the (-10,0] bucket.
	got := lines(run(t, e, "histogram_quantile(0.5, avg:h{*} by {upper_bound})", 0, 59, 60))
	if len(got) != 1 || got[0] != "*: 0" {
		t.Errorf("got %q, want the zero boundary", got)
	}
}

// timeshift moves the window it reads and keeps the timestamps it reports, so
// that a chart can draw an earlier period under the current one. The window
// here is the 60 seconds *after* the fixture's data, so a shift of -60 lands
// on it.
func TestEval_Timeshift(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		want        []string
	}{
		{
			"an earlier window, reported against this one",
			"timeshift(sum:req.count{route:/x} by {route}, -60)",
			[]string{"route:/x: 66,165"},
		},
		{
			"zero is the identity, and this window is empty",
			"timeshift(sum:req.count{route:/x} by {route}, 0)",
			nil,
		},
		{
			"a positive shift reads the future, which is empty",
			"timeshift(sum:req.count{route:/x} by {route}, 60)",
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := lines(run(t, fixture(), tc.query, 60, 119, 30))
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Errorf("\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The comparison timeshift exists for: this period over the one before it.
func TestEval_ThisPeriodOverLast(t *testing.T) {
	// Window [30,90) at 30s buckets. Now: 165 then nothing. Shifted back one
	// bucket: 66 then 165. So the first bucket is 165/66 and the second has
	// nothing to divide.
	res := run(t, fixture(),
		"sum:req.count{route:/x} by {route} / timeshift(sum:req.count{route:/x} by {route}, -30)", 30, 89, 30)
	if got := lines(res); len(got) != 1 || got[0] != "route:/x: 2.5,_" {
		t.Errorf("got %q, want route:/x: 2.5,_", got)
	}
}

// A line with no partner on the other side of an operator is dropped, and the
// drop is reported: silence about it is how a dashboard loses a service
// quietly. Here the current window has no data at all.
func TestEval_JoinAgainstNothingWarns(t *testing.T) {
	res := run(t, fixture(),
		"sum:req.count{route:/x} by {route} / timeshift(sum:req.count{route:/x} by {route}, -60)", 60, 119, 30)
	if len(res.Series) != 0 {
		t.Errorf("got %q, want nothing: there is no current data to divide", lines(res))
	}
	if len(res.Warnings) == 0 {
		t.Error("dropped every line without a warning")
	}
}

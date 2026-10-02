package openmetrics

import (
	"errors"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tuvo1106/ozymandias/internal/sketch"
)

var inf = math.Inf(1)

func oneFamily(t *testing.T, body string) Family {
	t.Helper()
	fams := parse(t, body, Options{})
	if len(fams) != 1 {
		t.Fatalf("want one family, got %d", len(fams))
	}
	return fams[0]
}

func TestFamily_Histograms(t *testing.T) {
	f := oneFamily(t, strings.Join([]string{
		"# TYPE lat histogram",
		// Out of order, two series interleaved, label order varies.
		`lat_bucket{path="/a",le="1"} 5`,
		`lat_bucket{le="0.5",path="/a"} 2`,
		`lat_bucket{path="/b",le="+Inf"} 1`,
		`lat_bucket{path="/a",le="+Inf"} 7`,
		`lat_sum{path="/a"} 4.5`,
		`lat_count{path="/a"} 7`,
		`lat_created{path="/a"} 1.7e9`,
		`lat_count{path="/b"} 1`,
		""}, "\n"))
	got, err := f.Histograms()
	if err != nil {
		t.Fatal(err)
	}
	want := []Histogram{
		{Labels: []Label{{"path", "/a"}}, Buckets: []Bucket{{0.5, 2}, {1, 5}, {inf, 7}}, Sum: 4.5, HasSum: true, Count: 7, HasCount: true},
		{Labels: []Label{{"path", "/b"}}, Buckets: []Bucket{{inf, 1}}, Count: 1, HasCount: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestFamily_Histograms_MissingInf(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       float64
	}{
		{"from _count", "# TYPE h histogram\nh_bucket{le=\"1\"} 3\nh_count 4\n", 4},
		// Without _count the largest cumulative count is the best bound; a
		// dip (non-atomic scrape) must not make it smaller.
		{"from the largest bucket", "# TYPE h histogram\nh_bucket{le=\"1\"} 3\nh_bucket{le=\"2\"} 2\n", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := oneFamily(t, tc.body)
			hs, err := f.Histograms()
			if err != nil {
				t.Fatal(err)
			}
			last := hs[0].Buckets[len(hs[0].Buckets)-1]
			if !math.IsInf(last.UpperBound, 1) || last.Count != tc.want {
				t.Fatalf("last bucket %+v, want +Inf with %v", last, tc.want)
			}
		})
	}
}

func TestFamily_Histograms_GaugeHistogram(t *testing.T) {
	f := oneFamily(t, "# TYPE g gaugehistogram\ng_bucket{le=\"-1\"} 1\ng_bucket{le=\"+Inf\"} 3\ng_gsum -2\ng_gcount 3\n# EOF\n")
	hs, err := f.Histograms()
	if err != nil {
		t.Fatal(err)
	}
	want := Histogram{Buckets: []Bucket{{-1, 1}, {inf, 3}}, Sum: -2, HasSum: true, Count: 3, HasCount: true}
	if len(hs) != 1 || !reflect.DeepEqual(hs[0], want) {
		t.Fatalf("got %+v", hs)
	}
}

func TestFamily_Histograms_Errors(t *testing.T) {
	for _, tc := range []struct{ name, body, msg string }{
		{"not a histogram", "# TYPE c counter\nc 1\n", "not a histogram"},
		{"bucket without le", "# TYPE h histogram\nh_bucket 1\n", "without an le"},
		{"le not a number", "# TYPE h histogram\nh_bucket{le=\"x\"} 1\n", "not a number"},
		{"le NaN", "# TYPE h histogram\nh_bucket{le=\"NaN\"} 1\n", "not a number"},
		{"duplicate le", "# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_bucket{le=\"1.0\"} 2\n", "two buckets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := oneFamily(t, tc.body)
			if _, err := f.Histograms(); err == nil || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err = %v, want %q", err, tc.msg)
			}
		})
	}
}

// manyBuckets is one histogram series with n buckets, le 0..n-1, in the
// order given by at.
func manyBuckets(n int, at func(i int) int) Family {
	f := Family{Name: "h", Type: TypeHistogram, Samples: make([]Sample, n)}
	for i := range n {
		f.Samples[i] = Sample{Name: "h_bucket", Labels: []Label{{"le", strconv.Itoa(at(i))}}, Value: float64(i)}
	}
	return f
}

// A target may serve as many buckets as the sample limit allows (200,000),
// and parsing cannot be cancelled once the scrape's timeout has passed, so
// finding duplicate buckets must not be quadratic: 200,000 took about 5s
// that way, and takes about 0.1s sorted, under -race.
func TestFamily_Histograms_ManyBuckets(t *testing.T) {
	const n = 200_000
	f := manyBuckets(n, func(i int) int { return n - 1 - i })
	start := time.Now()
	hs, err := f.Histograms()
	took := time.Since(start)
	if err != nil || len(hs) != 1 || len(hs[0].Buckets) != n+1 {
		t.Fatalf("err %v, %d histograms", err, len(hs))
	}
	if took > 2*time.Second {
		t.Fatalf("%d buckets took %v: quadratic?", n, took)
	}
	// A duplicate as far apart as the input allows is still found.
	f = manyBuckets(n, func(i int) int { return min(i, n-2) })
	_, err = f.Histograms()
	if err == nil || !strings.Contains(err.Error(), `two buckets with le="199998"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestBucketDeltas(t *testing.T) {
	prev := []Bucket{{0.1, 2}, {1, 5}, {inf, 6}}
	for _, tc := range []struct {
		name      string
		prev, cur []Bucket
		want      []Bucket
		reset     bool
	}{
		{"first scrape has no delta", nil, prev, nil, false},
		{"steady growth", prev, []Bucket{{0.1, 3}, {1, 9}, {inf, 12}}, []Bucket{{0.1, 1}, {1, 3}, {inf, 2}}, false},
		{"nothing happened", prev, prev, []Bucket{{0.1, 0}, {1, 0}, {inf, 0}}, false},
		// The process restarted and has counted 4 since: cur is the delta.
		{"restart", prev, []Bucket{{0.1, 1}, {1, 3}, {inf, 4}}, []Bucket{{0.1, 1}, {1, 2}, {inf, 1}}, true},
		// One bucket going down is enough, even if the total went up.
		{"restart visible in one bucket", prev, []Bucket{{0.1, 1}, {1, 7}, {inf, 9}}, []Bucket{{0.1, 1}, {1, 6}, {inf, 2}}, true},
		{"new layout", prev, []Bucket{{0.5, 3}, {inf, 9}}, []Bucket{{0.5, 3}, {inf, 6}}, true},
		{"new layout, same length", prev, []Bucket{{0.2, 3}, {1, 9}, {inf, 12}}, []Bucket{{0.2, 3}, {1, 6}, {inf, 3}}, true},
		// A torn read (bucket 1 read before an observation landed, bucket
		// 0.1 after): counts dip; no bucket goes negative and the total is
		// the largest cumulative delta.
		{"torn read", []Bucket{{0.1, 0}, {1, 0}, {inf, 0}}, []Bucket{{0.1, 3}, {1, 2}, {inf, 4}}, []Bucket{{0.1, 3}, {1, 0}, {inf, 1}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reset := BucketDeltas(tc.prev, tc.cur)
			if reset != tc.reset || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v reset=%v, want %+v reset=%v", got, reset, tc.want, tc.reset)
			}
		})
	}
	// cur must not be modified.
	cur := []Bucket{{0.1, 3}, {inf, 5}}
	BucketDeltas([]Bucket{{0.1, 1}, {inf, 1}}, cur)
	if cur[0].Count != 3 || cur[1].Count != 5 {
		t.Fatalf("BucketDeltas modified cur: %+v", cur)
	}
}

// linearQuantile is histogram_quantile()'s algorithm over per-bucket counts,
// as the reference ToSketch is meant to agree with.
func linearQuantile(q float64, deltas []Bucket) float64 {
	var total float64
	for _, b := range deltas {
		total += b.Count
	}
	rank := q * total
	var cum, lower float64
	for i, b := range deltas {
		if i == 0 && b.UpperBound > 0 {
			lower = 0
		}
		if cum+b.Count >= rank && b.Count > 0 {
			if math.IsInf(b.UpperBound, 1) {
				return lower
			}
			return lower + (b.UpperBound-lower)*(rank-cum)/b.Count
		}
		cum += b.Count
		lower = b.UpperBound
	}
	return lower
}

func TestToSketch_AgreesWithLinearInterpolation(t *testing.T) {
	// A typical latency layout with its counts spread unevenly.
	deltas := []Bucket{{0.005, 10}, {0.01, 40}, {0.025, 120}, {0.05, 300}, {0.1, 250}, {0.25, 180}, {0.5, 60}, {1, 30}, {2.5, 8}, {5, 2}, {inf, 0}}
	s := sketch.NewDefault()
	if err := ToSketch(deltas, s); err != nil {
		t.Fatal(err)
	}
	if s.Count() < 999.999 || s.Count() > 1000.001 {
		t.Fatalf("count %v, want 1000", s.Count())
	}
	for _, q := range []float64{0.5, 0.75, 0.9, 0.95, 0.99} {
		got, err := s.Quantile(q)
		if err != nil {
			t.Fatal(err)
		}
		want := linearQuantile(q, deltas)
		// Find the bucket width at want: the spreading adds up to half a
		// subdivision, the sketch its relative α on top.
		var width float64
		lower := 0.0
		for _, b := range deltas {
			if want <= b.UpperBound {
				width = b.UpperBound - lower
				break
			}
			lower = b.UpperBound
		}
		tol := width/Subdivisions + 2*s.Alpha()*want
		if math.Abs(got-want) > tol {
			t.Errorf("p%v: sketch %v, linear %v, |diff| %v > %v", q*100, got, want, math.Abs(got-want), tol)
		}
	}
}

func TestToSketch_Bounds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deltas   []Bucket
		min, max float64 // the sketch's observed range
	}{
		// First bucket with a positive bound starts at 0.
		{"first bucket from zero", []Bucket{{1, 4}, {inf, 0}}, 1.0 / 32, 31.0 / 32},
		// First bucket with a bound ≤ 0 has no lower bound: a point mass.
		{"first bucket non-positive", []Bucket{{-2, 3}, {0, 0}, {inf, 0}}, -2, -2},
		// Between negative bounds, spread like any other interval.
		{"negative interval", []Bucket{{-4, 0}, {-2, 16}, {inf, 0}}, -4 + 2.0/32, -2 - 2.0/32},
		{"interval straddling zero", []Bucket{{-1, 0}, {1, 16}, {inf, 0}}, -1 + 2.0/32, 1 - 2.0/32},
		// +Inf observations sit at the largest finite bound.
		{"+Inf at the last finite bound", []Bucket{{0.5, 0}, {1, 0}, {inf, 5}}, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sketch.NewDefault()
			if err := ToSketch(tc.deltas, s); err != nil {
				t.Fatal(err)
			}
			var n float64
			for _, b := range tc.deltas {
				n += b.Count
			}
			if math.Abs(s.Count()-n) > 1e-9 || math.Abs(s.Min()-tc.min) > 1e-12 || math.Abs(s.Max()-tc.max) > 1e-12 {
				t.Fatalf("count %v min %v max %v, want %v %v %v", s.Count(), s.Min(), s.Max(), n, tc.min, tc.max)
			}
		})
	}
}

func TestToSketch_Errors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		deltas []Bucket
		is     error
		msg    string
	}{
		{"only +Inf", []Bucket{{inf, 3}}, ErrNoFiniteBucket, ""},
		{"unsorted", []Bucket{{1, 1}, {0.5, 1}}, nil, "ascending"},
		{"duplicate bound", []Bucket{{1, 1}, {1, 1}}, nil, "ascending"},
		{"NaN bound", []Bucket{{math.NaN(), 1}}, nil, "not usable"},
		{"negative count", []Bucket{{1, -1}}, nil, "not usable"},
		{"infinite count", []Bucket{{1, inf}}, nil, "not usable"},
		{"-Inf bound", []Bucket{{math.Inf(-1), 1}}, sketch.ErrNotFinite, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ToSketch(tc.deltas, sketch.NewDefault())
			if err == nil || (tc.is != nil && !errors.Is(err, tc.is)) || !strings.Contains(err.Error(), tc.msg) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	// Only +Inf, but empty: nothing to place, so no error.
	if err := ToSketch([]Bucket{{inf, 0}}, sketch.NewDefault()); err != nil {
		t.Fatal(err)
	}
}

// LabelKey is order-blind, leaves out the label it is told to, and cannot
// be forged: a value carrying separator bytes or a length-looking prefix
// still differs from the label set it imitates. The parser does not
// require valid UTF-8, so a page can carry any byte in a value.
func TestLabelKey(t *testing.T) {
	ab := []Label{{"a", "1"}, {"b", "2"}}
	if LabelKey(ab, "") != LabelKey([]Label{{"b", "2"}, {"a", "1"}}, "") {
		t.Error("label order changed the key")
	}
	if LabelKey(append(ab, Label{"le", "0.5"}), "le") != LabelKey(ab, "") {
		t.Error("the skipped label changed the key")
	}
	for _, forged := range [][]Label{
		{{"a", "x\xfeb\xffc"}},
		{{"a", "x\xffb\xfec"}},
		{{"a", "x1:b1:c"}},
		{{"a", "x"}, {"b", "c"}},
		{{"a1:x1:b", "c"}},
	} {
		for _, other := range [][]Label{{{"a", "x"}, {"b", "c"}}, {{"a", "x\xfeb\xffc"}}} {
			if slices.Equal(forged, other) {
				continue
			}
			if LabelKey(forged, "") == LabelKey(other, "") {
				t.Errorf("%q and %q have one key", forged, other)
			}
		}
	}
}

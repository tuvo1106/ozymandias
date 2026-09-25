package sketch

import (
	"errors"
	"math"
	"math/rand"
	"sort"
	"testing"
)

// exactQuantile is the nearest-rank quantile of the raw values, which is what
// the sketch's rank convention q*(n-1) selects: the walk stops at the first
// bucket whose cumulative count exceeds q*(n-1), which is the observation at
// index int(q*(n-1)) of the sorted input.
func exactQuantile(values []float64, q float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if q <= 0 {
		return sorted[0]
	}
	if q >= 1 {
		return sorted[len(sorted)-1]
	}
	return sorted[int(q*float64(len(sorted)-1))]
}

// withinAlpha reports whether got is within a relative error of alpha of want.
func withinAlpha(got, want, alpha float64) bool {
	if want == 0 {
		return got == 0
	}
	// A hair of slack for the floating-point error in pow/log, which is not
	// what the guarantee is about.
	return math.Abs(got-want) <= alpha*math.Abs(want)*(1+1e-9)+1e-12
}

func mustAdd(t *testing.T, s *Sketch, values ...float64) {
	t.Helper()
	for _, v := range values {
		if err := s.Add(v); err != nil {
			t.Fatalf("Add(%v): %v", v, err)
		}
	}
}

// TestSketch_QuantilesAreWithinAlpha is the guarantee the package exists for,
// checked against exact answers over the distribution shapes that break naive
// histograms: a long tail, a tight cluster, and a range spanning many orders
// of magnitude at once.
func TestSketch_QuantilesAreWithinAlpha(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, tc := range []struct {
		name string
		gen  func(i int) float64
		n    int
	}{
		{"uniform 1..1000", func(i int) float64 { return float64(i%1000) + 1 }, 10_000},
		{"exponential tail", func(int) float64 { return rng.ExpFloat64() * 100 }, 10_000},
		{"tight cluster", func(int) float64 { return 100 + rng.NormFloat64() }, 10_000},
		{"eight orders of magnitude", func(i int) float64 { return math.Pow(10, float64(i%8)) }, 8_000},
		{"one observation", func(int) float64 { return 42 }, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewDefault()
			values := make([]float64, tc.n)
			for i := range values {
				values[i] = tc.gen(i)
				if values[i] <= 0 {
					values[i] = 1e-6 // the generators above are positive by intent
				}
			}
			mustAdd(t, s, values...)

			for _, q := range []float64{0, 0.01, 0.25, 0.5, 0.75, 0.9, 0.95, 0.99, 1} {
				got, err := s.Quantile(q)
				if err != nil {
					t.Fatalf("q=%v: %v", q, err)
				}
				want := exactQuantile(values, q)
				if !withinAlpha(got, want, DefaultAlpha) {
					t.Errorf("q=%v: got %v, exact %v — relative error %v exceeds alpha %v",
						q, got, want, math.Abs(got-want)/math.Abs(want), DefaultAlpha)
				}
			}
		})
	}
}

// TestSketch_ExtremesAreExact. Count, sum, min and max are tracked outside the
// buckets, so they are not estimates — and that is what makes Quantile(0) and
// Quantile(1) exact. Anyone who distrusts a sketch checks the ends first.
func TestSketch_ExtremesAreExact(t *testing.T) {
	s := NewDefault()
	values := []float64{2.1, 96.0, 17.5, 3.3, 40.25}
	mustAdd(t, s, values...)

	if got := s.Count(); got != 5 {
		t.Errorf("Count = %v, want 5", got)
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	if got := s.Sum(); math.Abs(got-sum) > 1e-9 {
		t.Errorf("Sum = %v, want %v", got, sum)
	}
	if got := s.Min(); got != 2.1 {
		t.Errorf("Min = %v, want 2.1", got)
	}
	if got := s.Max(); got != 96.0 {
		t.Errorf("Max = %v, want 96", got)
	}
	for q, want := range map[float64]float64{0: 2.1, 1: 96.0} {
		got, err := s.Quantile(q)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("Quantile(%v) = %v, want exactly %v", q, got, want)
		}
	}
}

// TestSketch_ZeroAndNegativeValues. log of zero is undefined and log of a
// negative is worse, so both need handling that is not "put it in the lowest
// bucket". Zero gets its own counter; negatives get a mirrored store indexed
// by absolute value, which means its indices run backwards relative to the
// values they describe.
func TestSketch_ZeroAndNegativeValues(t *testing.T) {
	s := NewDefault()
	values := []float64{-100, -10, -1, 0, 0, 1, 10, 100}
	mustAdd(t, s, values...)

	if got := s.ZeroCount(); got != 2 {
		t.Errorf("ZeroCount = %v, want 2", got)
	}
	if got := s.Count(); got != 8 {
		t.Errorf("Count = %v, want 8", got)
	}
	if got := s.Min(); got != -100 {
		t.Errorf("Min = %v, want -100", got)
	}
	if got := s.Max(); got != 100 {
		t.Errorf("Max = %v, want 100", got)
	}
	for _, q := range []float64{0, 0.125, 0.25, 0.5, 0.625, 0.75, 1} {
		got, err := s.Quantile(q)
		if err != nil {
			t.Fatal(err)
		}
		want := exactQuantile(values, q)
		if !withinAlpha(got, want, DefaultAlpha) {
			t.Errorf("q=%v: got %v, exact %v", q, got, want)
		}
	}
}

// TestSketch_MergeEqualsSketchOfTheConcatenation is the property that makes
// the whole architecture work: a query merges sketches from many hosts and
// many intervals, and must get the answer a single sketch of every observation
// would have given.
func TestSketch_MergeEqualsSketchOfTheConcatenation(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	a, b := NewDefault(), NewDefault()
	both := NewDefault()
	var all []float64
	for i := 0; i < 5_000; i++ {
		v := rng.ExpFloat64()*50 + 0.1
		all = append(all, v)
		if i%2 == 0 {
			mustAdd(t, a, v)
		} else {
			mustAdd(t, b, v)
		}
		mustAdd(t, both, v)
	}
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}

	if a.Count() != both.Count() {
		t.Errorf("merged count %v, want %v", a.Count(), both.Count())
	}
	if math.Abs(a.Sum()-both.Sum()) > 1e-6 {
		t.Errorf("merged sum %v, want %v", a.Sum(), both.Sum())
	}
	if a.Min() != both.Min() || a.Max() != both.Max() {
		t.Errorf("merged range [%v,%v], want [%v,%v]", a.Min(), a.Max(), both.Min(), both.Max())
	}
	for _, q := range []float64{0, 0.5, 0.95, 0.99, 1} {
		got, _ := a.Quantile(q)
		want, _ := both.Quantile(q)
		if got != want {
			t.Errorf("q=%v: merged gives %v, one sketch of everything gives %v", q, got, want)
		}
		if exact := exactQuantile(all, q); !withinAlpha(got, exact, DefaultAlpha) {
			t.Errorf("q=%v: merged %v is outside alpha of exact %v", q, got, exact)
		}
	}
}

func TestSketch_MergeRejectsADifferentGamma(t *testing.T) {
	a, b := New(0.01), New(0.02)
	if err := a.Merge(b); !errors.Is(err, ErrIncompatible) {
		t.Errorf("Merge across gammas = %v, want ErrIncompatible", err)
	}
	// A nil merge is a no-op rather than a panic: a query that found no
	// sketches for one series should not have to special-case that.
	if err := a.Merge(nil); err != nil {
		t.Errorf("Merge(nil) = %v, want nil", err)
	}
}

func TestSketch_MergeDoesNotAliasItsSource(t *testing.T) {
	a, b := NewDefault(), NewDefault()
	mustAdd(t, a, 1, 2, 3)
	mustAdd(t, b, 10, 20, 30)
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	// b must be untouched: a query merges the same stored sketch into several
	// groups, and a merge that wrote through would corrupt the second one.
	if b.Count() != 3 {
		t.Errorf("the source sketch now has count %v, want 3", b.Count())
	}
	mustAdd(t, a, 1000)
	if b.Max() != 30 {
		t.Errorf("adding to the merged sketch changed the source: max %v, want 30", b.Max())
	}
}

func TestSketch_EmptySketch(t *testing.T) {
	s := NewDefault()
	if got := s.Count(); got != 0 {
		t.Errorf("Count = %v, want 0", got)
	}
	// NaN rather than 0: there is no value to be within alpha of, and 0 is a
	// plausible-looking lie that a dashboard would plot.
	got, err := s.Quantile(0.5)
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(got) {
		t.Errorf("Quantile on an empty sketch = %v, want NaN", got)
	}
}

func TestSketch_RejectsBadInput(t *testing.T) {
	s := NewDefault()
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := s.Add(v); !errors.Is(err, ErrNotFinite) {
			t.Errorf("Add(%v) = %v, want ErrNotFinite", v, err)
		}
	}
	for _, w := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if err := s.AddWithCount(1, w); err == nil {
			t.Errorf("AddWithCount with weight %v was accepted", w)
		}
	}
	for _, q := range []float64{-0.1, 1.1, math.NaN()} {
		if _, err := s.Quantile(q); !errors.Is(err, ErrBadQuantile) {
			t.Errorf("Quantile(%v) = %v, want ErrBadQuantile", q, err)
		}
	}
	if s.Count() != 0 {
		t.Errorf("rejected input still counted: %v", s.Count())
	}
}

func TestNew_RejectsAnImpossibleAlpha(t *testing.T) {
	for _, alpha := range []float64{0, 1, -0.5, 2} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("New(%v) did not panic", alpha)
				}
			}()
			New(alpha)
		}()
	}
}

func TestNewWithGamma(t *testing.T) {
	// The wire carries gamma, not alpha, so a decoder rebuilds from it — and
	// the round trip must land on the same bucket boundaries.
	want := NewDefault()
	got, err := NewWithGamma(want.Gamma())
	if err != nil {
		t.Fatal(err)
	}
	mustAdd(t, want, 1, 10, 100, 1000)
	mustAdd(t, got, 1, 10, 100, 1000)
	for i, b := range got.PositiveBins() {
		if w := want.PositiveBins()[i]; b != w {
			t.Errorf("bin %d = %+v, want %+v", i, b, w)
		}
	}
	if math.Abs(got.Alpha()-DefaultAlpha) > 1e-12 {
		t.Errorf("Alpha() = %v, want %v", got.Alpha(), DefaultAlpha)
	}
	for _, g := range []float64{1, 0.5, 0, -1, math.NaN(), math.Inf(1)} {
		if _, err := NewWithGamma(g); err == nil {
			t.Errorf("NewWithGamma(%v) was accepted", g)
		}
	}
}

// TestSketch_SampledObservationsWeighNonIntegerAmounts. A statsd client
// sampling at 0.1 sends one packet meaning ten observations, so weights are
// fractional throughout and count is a float64 rather than an integer.
func TestSketch_SampledObservationsWeighNonIntegerAmounts(t *testing.T) {
	s := NewDefault()
	if err := s.AddWithCount(100, 10); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWithCount(200, 2.5); err != nil {
		t.Fatal(err)
	}
	if got := s.Count(); got != 12.5 {
		t.Errorf("Count = %v, want 12.5", got)
	}
	if got, want := s.Sum(), 100*10+200*2.5; got != want {
		t.Errorf("Sum = %v, want %v", got, want)
	}
	got, err := s.Quantile(0.5)
	if err != nil {
		t.Fatal(err)
	}
	// Ten of the twelve and a half observations are 100, so the median is.
	if !withinAlpha(got, 100, DefaultAlpha) {
		t.Errorf("median = %v, want ~100", got)
	}
}

// Values spanning more than the store can index — a nanosecond and a century in
// the same metric — must fold rather than panic, and must fold at the end
// nobody queries. The high quantiles stay accurate; the low ones do not, which
// is the trade the collapse exists to make.
func TestSketch_ValuesBeyondTheIndexableSpan(t *testing.T) {
	s := NewDefault()
	mustAdd(t, s, 1e-9)
	for i := 0; i < 100; i++ {
		mustAdd(t, s, 1e9)
	}

	if got, want := s.Count(), float64(101); got != want {
		t.Fatalf("count: got %v, want %v", got, want)
	}
	if got := s.Min(); got != 1e-9 {
		t.Errorf("min: got %v, want 1e-9 — the aggregates are exact regardless of collapse", got)
	}
	if got := s.Max(); got != 1e9 {
		t.Errorf("max: got %v, want 1e9", got)
	}
	p95, err := s.Quantile(0.95)
	if err != nil {
		t.Fatalf("Quantile: %v", err)
	}
	if !withinAlpha(p95, 1e9, DefaultAlpha) {
		t.Errorf("p95: got %v, want 1e9 within %v relative error", p95, DefaultAlpha)
	}
}

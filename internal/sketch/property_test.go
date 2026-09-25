package sketch

import (
	"math"
	"sort"
	"testing"

	"pgregory.net/rapid"
)

// genValue draws observations across a wide but realistic range. Latencies,
// payload sizes and queue depths all live here, and the range deliberately
// spans several orders of magnitude, because that is the case a fixed-width
// histogram cannot serve and this one must.
func genValue() *rapid.Generator[float64] {
	return rapid.Custom(func(t *rapid.T) float64 {
		exp := rapid.IntRange(-6, 9).Draw(t, "exp")
		mant := rapid.Float64Range(1, 10).Draw(t, "mant")
		if rapid.Bool().Draw(t, "negative") {
			return -mant * math.Pow(10, float64(exp))
		}
		return mant * math.Pow(10, float64(exp))
	})
}

func sketchOf(t *rapid.T, values []float64) *Sketch {
	s := NewDefault()
	for _, v := range values {
		if err := s.Add(v); err != nil {
			t.Fatalf("Add(%v): %v", v, err)
		}
	}
	return s
}

// TestProperty_QuantileIsWithinAlpha. The one guarantee the package makes, on
// arbitrary input rather than the shapes a test author thought of.
func TestProperty_QuantileIsWithinAlpha(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		values := rapid.SliceOfN(genValue(), 1, 500).Draw(t, "values")
		q := rapid.Float64Range(0, 1).Draw(t, "q")
		s := sketchOf(t, values)

		got, err := s.Quantile(q)
		if err != nil {
			t.Fatalf("Quantile(%v): %v", q, err)
		}
		sorted := append([]float64(nil), values...)
		sort.Float64s(sorted)
		want := sorted[int(q*float64(len(sorted)-1))]
		if q >= 1 {
			want = sorted[len(sorted)-1]
		}
		if !withinAlpha(got, want, DefaultAlpha) {
			t.Fatalf("q=%v over %d values: got %v, exact %v (relative error %v)",
				q, len(values), got, want, math.Abs(got-want)/math.Abs(want))
		}
	})
}

// TestProperty_MergeEqualsSketchOfTheConcatenation. Splitting observations
// across sketches and merging must be indistinguishable from never having
// split them — this is what lets a query combine hosts and intervals freely.
func TestProperty_MergeEqualsSketchOfTheConcatenation(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		left := rapid.SliceOfN(genValue(), 0, 200).Draw(t, "left")
		right := rapid.SliceOfN(genValue(), 0, 200).Draw(t, "right")
		if len(left)+len(right) == 0 {
			return
		}

		merged := sketchOf(t, left)
		if err := merged.Merge(sketchOf(t, right)); err != nil {
			t.Fatalf("Merge: %v", err)
		}
		whole := sketchOf(t, append(append([]float64(nil), left...), right...))

		if merged.Count() != whole.Count() {
			t.Fatalf("count %v, want %v", merged.Count(), whole.Count())
		}
		if merged.Min() != whole.Min() || merged.Max() != whole.Max() {
			t.Fatalf("range [%v,%v], want [%v,%v]",
				merged.Min(), merged.Max(), whole.Min(), whole.Max())
		}
		// Sums are added in a different order, so they may differ in the last
		// bits; everything else must match exactly.
		if d := math.Abs(merged.Sum() - whole.Sum()); d > 1e-6*math.Max(1, math.Abs(whole.Sum())) {
			t.Fatalf("sum %v, want %v", merged.Sum(), whole.Sum())
		}
		for _, q := range []float64{0, 0.1, 0.5, 0.9, 0.99, 1} {
			a, _ := merged.Quantile(q)
			b, _ := whole.Quantile(q)
			if a != b {
				t.Fatalf("q=%v: merged %v, whole %v", q, a, b)
			}
		}
	})
}

// TestProperty_MergeIsCommutativeAndAssociative. A query merges whatever the
// storage hands it, in whatever order — by host, by block, by time — so the
// order must not be visible in the answer.
func TestProperty_MergeIsCommutativeAndAssociative(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := rapid.SliceOfN(genValue(), 0, 100).Draw(t, "a")
		b := rapid.SliceOfN(genValue(), 0, 100).Draw(t, "b")
		c := rapid.SliceOfN(genValue(), 0, 100).Draw(t, "c")
		if len(a)+len(b)+len(c) == 0 {
			return
		}

		// (a ∪ b) ∪ c
		left := sketchOf(t, a)
		_ = left.Merge(sketchOf(t, b))
		_ = left.Merge(sketchOf(t, c))

		// c ∪ (b ∪ a) — different order, different association
		right := sketchOf(t, c)
		bc := sketchOf(t, b)
		_ = bc.Merge(sketchOf(t, a))
		_ = right.Merge(bc)

		if left.Count() != right.Count() {
			t.Fatalf("count %v vs %v", left.Count(), right.Count())
		}
		if left.Min() != right.Min() || left.Max() != right.Max() {
			t.Fatalf("range [%v,%v] vs [%v,%v]", left.Min(), left.Max(), right.Min(), right.Max())
		}
		for _, q := range []float64{0, 0.25, 0.5, 0.75, 0.95, 1} {
			x, _ := left.Quantile(q)
			y, _ := right.Quantile(q)
			if x != y {
				t.Fatalf("q=%v: %v vs %v — merge order is visible in the answer", q, x, y)
			}
		}
	})
}

// TestProperty_BinsRoundTripThroughTheAccessors. The wire encoder and the
// on-disk encoding both rebuild a sketch from its bins plus its aggregates, so
// that path has to reproduce the original exactly — not within alpha, exactly,
// because no new bucketing happens.
func TestProperty_BinsRoundTripThroughTheAccessors(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		values := rapid.SliceOfN(genValue(), 1, 300).Draw(t, "values")
		orig := sketchOf(t, values)

		rebuilt, err := NewWithGamma(orig.Gamma())
		if err != nil {
			t.Fatalf("NewWithGamma: %v", err)
		}
		for _, b := range orig.PositiveBins() {
			if err := rebuilt.AddBin(b, false); err != nil {
				t.Fatalf("AddBin(%+v): %v", b, err)
			}
		}
		for _, b := range orig.NegativeBins() {
			if err := rebuilt.AddBin(b, true); err != nil {
				t.Fatalf("AddBin(%+v, negative): %v", b, err)
			}
		}
		if err := rebuilt.SetAggregates(orig.Count(), orig.Sum(), orig.Min(), orig.Max(), orig.ZeroCount()); err != nil {
			t.Fatalf("SetAggregates: %v", err)
		}

		for _, q := range []float64{0, 0.1, 0.5, 0.9, 0.99, 1} {
			a, _ := orig.Quantile(q)
			b, _ := rebuilt.Quantile(q)
			if a != b {
				t.Fatalf("q=%v: original %v, rebuilt %v", q, a, b)
			}
		}
	})
}

// The store's own invariants, driven by raw bucket indices rather than values.
//
// This exists because genValue above cannot reach the collapse: its range spans
// 10^15, and MaxBins buckets at the default gamma span nearer 10^17, so every
// sketch the value-level properties build fits comfortably. Indices reach it in
// two adds. No weight is ever lost — collapse folds counts into the floor
// bucket, it does not drop them.
func TestProperty_StoreKeepsItsInvariants(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		indices := rapid.SliceOfN(rapid.IntRange(-20_000, 20_000), 1, 200).Draw(t, "indices")

		var s store
		var added float64
		for _, k := range indices {
			w := rapid.Float64Range(0.5, 4).Draw(t, "weight")
			s.add(k, w)
			added += w
		}

		if len(s.counts) > MaxBins {
			t.Fatalf("store grew to %d buckets, past the %d cap", len(s.counts), MaxBins)
		}
		var summed float64
		s.forEach(func(_ int, c float64) { summed += c })
		if math.Abs(summed-added) > 1e-9*added {
			t.Fatalf("counts sum to %v, but %v was added — collapse lost weight", summed, added)
		}
		if math.Abs(s.total-added) > 1e-9*added {
			t.Fatalf("total is %v, but %v was added", s.total, added)
		}
	})
}

// Merging must be indistinguishable from adding the source bucket by bucket,
// including when the union is wider than MaxBins and the destination has to
// collapse. Merge pre-sizes the destination in one step, so it reaches that
// decision by a different route than repeated adds do.
func TestProperty_MergeAgreesWithBucketByBucketAdds(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dstIdx := rapid.SliceOfN(rapid.IntRange(-20_000, 20_000), 0, 50).Draw(t, "dst")
		srcIdx := rapid.SliceOfN(rapid.IntRange(-20_000, 20_000), 1, 50).Draw(t, "src")

		var merged, oneByOne, src store
		for _, k := range dstIdx {
			merged.add(k, 1)
			oneByOne.add(k, 1)
		}
		for _, k := range srcIdx {
			src.add(k, 1)
		}

		merged.merge(&src)
		src.forEach(func(k int, c float64) { oneByOne.add(k, c) })

		gotBins, wantBins := merged.bins(), oneByOne.bins()
		if len(gotBins) != len(wantBins) {
			t.Fatalf("non-empty buckets: merge %v, adds %v", gotBins, wantBins)
		}
		for i := range gotBins {
			if gotBins[i] != wantBins[i] {
				t.Fatalf("bucket %d: merge %+v, adds %+v", i, gotBins[i], wantBins[i])
			}
		}
		if merged.total != oneByOne.total {
			t.Fatalf("total: merge %v, adds %v", merged.total, oneByOne.total)
		}
	})
}

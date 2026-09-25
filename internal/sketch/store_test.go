package sketch

import (
	"math"
	"testing"
)

// TestStore_CollapsesTheLowestBucketsNotTheHighest.
//
// The cap has to give somewhere, and which end it gives at is the one design
// decision in the store. A latency sketch is asked about p95 and p99; nobody
// pages on p1. So the low end folds together and the tail keeps its guarantee
// — the opposite choice would make the sketch precise about exactly the part
// of the distribution nobody queries.
func TestStore_CollapsesTheLowestBucketsNotTheHighest(t *testing.T) {
	var s store
	// One more bucket than the cap, added low to high, so the overflow is
	// forced at the top and the fold has to happen at the bottom.
	for k := 0; k <= MaxBins; k++ {
		s.add(k, 1)
	}
	if len(s.counts) != MaxBins {
		t.Fatalf("store holds %d buckets, want the cap of %d", len(s.counts), MaxBins)
	}
	if s.total != float64(MaxBins+1) {
		t.Errorf("total = %v, want %v — collapsing must fold counts, not drop them",
			s.total, MaxBins+1)
	}
	// The two lowest buckets are now one, holding both counts.
	if s.counts[0] != 2 {
		t.Errorf("the floor bucket holds %v, want 2 (its own count plus the one folded in)",
			s.counts[0])
	}
	// The top is untouched and still separable.
	if got := s.counts[len(s.counts)-1]; got != 1 {
		t.Errorf("the top bucket holds %v, want 1 — the tail must not be collapsed", got)
	}
	if s.offset != 1 {
		t.Errorf("offset = %d, want 1", s.offset)
	}
}

// TestStore_CollapsesWhenGrowingDownwards is the mirror case: a value arriving
// far below everything already stored. It cannot extend the store past the
// cap, so it folds into the floor — and is still counted.
func TestStore_CollapsesWhenGrowingDownwards(t *testing.T) {
	var s store
	for k := 0; k < MaxBins; k++ {
		s.add(k, 1)
	}
	s.add(-1000, 5)

	if len(s.counts) != MaxBins {
		t.Fatalf("store holds %d buckets, want %d", len(s.counts), MaxBins)
	}
	if s.total != float64(MaxBins)+5 {
		t.Errorf("total = %v, want %v — a value below the floor is still counted",
			s.total, float64(MaxBins)+5)
	}
	if s.counts[0] != 6 {
		t.Errorf("the floor bucket holds %v, want 6", s.counts[0])
	}
}

// TestStore_GrowsWithoutCollapsingInsideTheCap. The common case: everything
// fits, nothing is folded, and no count moves bucket.
func TestStore_GrowsWithoutCollapsingInsideTheCap(t *testing.T) {
	var s store
	s.add(100, 1)
	s.add(50, 2)  // downwards
	s.add(150, 3) // upwards
	if s.offset != 50 || len(s.counts) != 101 {
		t.Fatalf("store spans [%d, %d), want [50, 151)", s.offset, s.offset+len(s.counts))
	}
	for k, want := range map[int]float64{50: 2, 100: 1, 150: 3} {
		if got := s.counts[k-s.offset]; got != want {
			t.Errorf("bucket %d holds %v, want %v", k, got, want)
		}
	}
	if s.total != 6 {
		t.Errorf("total = %v, want 6", s.total)
	}
}

func TestStore_ForEachSkipsEmptyBucketsAndOrdersCorrectly(t *testing.T) {
	var s store
	s.add(10, 1)
	s.add(14, 2)
	var asc []int
	s.forEach(func(k int, c float64) { asc = append(asc, k) })
	if len(asc) != 2 || asc[0] != 10 || asc[1] != 14 {
		t.Errorf("forEach visited %v, want [10 14] and nothing between", asc)
	}
	var desc []int
	s.forEachDesc(func(k int, c float64) { desc = append(desc, k) })
	if len(desc) != 2 || desc[0] != 14 || desc[1] != 10 {
		t.Errorf("forEachDesc visited %v, want [14 10]", desc)
	}
}

func TestStore_CloneDoesNotShareBacking(t *testing.T) {
	var s store
	s.add(1, 1)
	c := s.clone()
	s.add(1, 10)
	if c.counts[0] != 1 || c.total != 1 {
		t.Errorf("the clone changed with its source: counts[0]=%v total=%v", c.counts[0], c.total)
	}
	var empty store
	if ec := empty.clone(); ec.counts != nil || !ec.isEmpty() {
		t.Errorf("cloning an empty store produced %+v", ec)
	}
}

// TestSketch_CollapseKeepsTheAggregatesExact. Collapsing loses the ability to
// separate low values from each other; it must not lose the values. Count,
// sum, min and max live outside the buckets precisely so that a collapsed
// sketch still answers those exactly, and so Quantile(0) is still the real
// minimum however far below the floor it fell.
func TestSketch_CollapseKeepsTheAggregatesExact(t *testing.T) {
	s := NewDefault()
	// A range wide enough to need more than MaxBins buckets: at gamma ~1.02,
	// 2048 buckets span about 10^17, so 10^-12 to 10^12 overflows it.
	values := []float64{1e-12, 1e-6, 1, 1e6, 1e12}
	var sum float64
	for _, v := range values {
		if err := s.Add(v); err != nil {
			t.Fatal(err)
		}
		sum += v
	}

	if got := s.Count(); got != float64(len(values)) {
		t.Errorf("Count = %v, want %v", got, len(values))
	}
	if got := s.Sum(); math.Abs(got-sum) > 1e-6 {
		t.Errorf("Sum = %v, want %v", got, sum)
	}
	if got := s.Min(); got != 1e-12 {
		t.Errorf("Min = %v, want 1e-12 — collapsing must not move the exact minimum", got)
	}
	if got := s.Max(); got != 1e12 {
		t.Errorf("Max = %v, want 1e12", got)
	}
	q0, _ := s.Quantile(0)
	q1, _ := s.Quantile(1)
	if q0 != 1e-12 || q1 != 1e12 {
		t.Errorf("extremes after collapse: [%v, %v], want [1e-12, 1e12]", q0, q1)
	}
	// And the top of the distribution still has its guarantee. With five
	// observations the rank for q=0.99 is 0.99*(5-1) = 3.96, so the answer is
	// the fourth value and not the fifth — the collapse is at the *other* end,
	// which is the point.
	for _, q := range []float64{0.5, 0.75, 0.99} {
		got, _ := s.Quantile(q)
		if want := exactQuantile(values, q); !withinAlpha(got, want, DefaultAlpha) {
			t.Errorf("q=%v after collapse = %v, want within alpha of %v", q, got, want)
		}
	}
}

// TestSketch_IndexIsClampedForUnrepresentableValues. A subnormal has a log so
// negative that the bucket index would not fit in an int on a 32-bit build,
// and the conversion would be undefined. Clamping keeps it defined; the value
// is still counted and still sets the minimum.
func TestSketch_IndexIsClampedForUnrepresentableValues(t *testing.T) {
	s := NewDefault()
	for _, v := range []float64{math.SmallestNonzeroFloat64, math.MaxFloat64} {
		if err := s.Add(v); err != nil {
			t.Fatalf("Add(%v): %v", v, err)
		}
	}
	if got := s.Count(); got != 2 {
		t.Errorf("Count = %v, want 2", got)
	}
	if got := s.Min(); got != math.SmallestNonzeroFloat64 {
		t.Errorf("Min = %v, want the smallest subnormal", got)
	}
	if got := s.Max(); got != math.MaxFloat64 {
		t.Errorf("Max = %v, want MaxFloat64", got)
	}
}

// growHigh reuses spare capacity instead of reallocating. The hazard in that is
// stale data: reslicing exposes elements the store has not written this time
// round. They can only be zero (length never shrinks, and every reallocation
// starts from a fresh make), and this pins that.
func TestStore_ReusedCapacityExposesOnlyZeroes(t *testing.T) {
	var s store
	s.add(100, 1)
	s.add(140, 1) // grows with spare capacity beyond index 140
	s.add(120, 7) // lands inside the run
	s.add(150, 1) // likely served from the spare capacity

	want := map[int]float64{100: 1, 120: 7, 140: 1, 150: 1}
	got := map[int]float64{}
	s.forEach(func(k int, c float64) { got[k] = c })

	if len(got) != len(want) {
		t.Fatalf("non-empty buckets: got %v, want %v", got, want)
	}
	for k, c := range want {
		if got[k] != c {
			t.Errorf("bucket %d: got %v, want %v", k, got[k], c)
		}
	}
	if s.total != 10 {
		t.Errorf("total: got %v, want 10", s.total)
	}
}

// merge sizes the destination once up front. That pre-sizing has to agree with
// what the per-bucket adds then do, including when the union of the two stores
// is wider than MaxBins and the low end has to collapse.
func TestStore_MergeWiderThanMaxBinsCollapsesLikeAdds(t *testing.T) {
	var merged, oneByOne store
	var src store
	for k := 0; k < MaxBins+500; k += 2 {
		src.add(k, 1)
	}

	merged.add(MaxBins+600, 3)
	oneByOne.add(MaxBins+600, 3)

	merged.merge(&src)
	src.forEach(func(k int, c float64) { oneByOne.add(k, c) })

	sameContents(t, &merged, &oneByOne)
}

// An all-zero source has no buckets to merge, and must not leave the
// destination looking as though it had been written to.
func TestStore_MergingAnEmptySourceIsANoOp(t *testing.T) {
	var dst, empty store
	dst.merge(&empty)
	if !dst.isEmpty() {
		t.Fatalf("merging nothing into nothing left a non-empty store: %+v", dst)
	}

	zeroed := store{counts: []float64{0, 0, 0}, offset: 5}
	dst.merge(&zeroed)
	if !dst.isEmpty() {
		t.Fatalf("merging all-zero buckets left a non-empty store: %+v", dst)
	}
}

// A narrow store asked to cover a bucket far below it is the case where the
// number of buckets falling off the bottom is zero but the span still exceeds
// MaxBins. Everything already held survives; only the new value folds into the
// floor.
func TestStore_GrowingFarBelowANarrowStore(t *testing.T) {
	var s store
	s.add(10_000, 5)
	s.add(10_001, 2)

	s.add(1, 3) // more than MaxBins below the existing pair

	if got, want := len(s.counts), MaxBins; got != want {
		t.Fatalf("len: got %d, want %d", got, want)
	}
	if got, want := s.offset, 10_001-MaxBins+1; got != want {
		t.Fatalf("offset: got %d, want %d", got, want)
	}
	if got := s.counts[0]; got != 3 {
		t.Errorf("floor bucket absorbed %v, want the 3 that fell below it", got)
	}
	if got := s.counts[10_000-s.offset]; got != 5 {
		t.Errorf("bucket 10000: got %v, want 5 — an existing bucket was lost", got)
	}
	if got := s.counts[10_001-s.offset]; got != 2 {
		t.Errorf("bucket 10001: got %v, want 2 — an existing bucket was lost", got)
	}
	if s.total != 10 {
		t.Errorf("total: got %v, want 10", s.total)
	}
}

// merge pre-sizes the destination from the source's first and last non-empty
// buckets, not from its offset. Those differ when the source carries leading
// empty buckets — which a store built only by add never does, so the case
// needs constructing rather than growing.
func TestStore_MergeFromASourceWithLeadingEmptyBuckets(t *testing.T) {
	src := store{counts: []float64{0, 0, 0, 5, 0, 2}, offset: 1_000, total: 7}

	var merged, oneByOne store
	merged.add(1_010, 1)
	oneByOne.add(1_010, 1)

	merged.merge(&src)
	src.forEach(func(k int, c float64) { oneByOne.add(k, c) })

	sameContents(t, &merged, &oneByOne)
	if merged.total != 8 {
		t.Errorf("total: got %v, want 8", merged.total)
	}
}

// sameContents fails unless two stores hold the same non-empty buckets and the
// same total.
//
// Shape — the offset, and the empty buckets a particular growth path happens to
// leave at either end — is representation, not content. growLow pads
// geometrically, so a store grown in one step and the same store grown one add
// at a time hold identical data in different widths. Every walk skips the empty
// buckets and every encoding omits them, so nothing above this file can tell
// the two apart.
func sameContents(t *testing.T, got, want *store) {
	t.Helper()
	gotBins, wantBins := got.bins(), want.bins()
	if len(gotBins) != len(wantBins) {
		t.Fatalf("non-empty buckets: got %v, want %v", gotBins, wantBins)
	}
	for i := range gotBins {
		if gotBins[i] != wantBins[i] {
			t.Fatalf("bucket %d of %d: got %+v, want %+v", i, len(gotBins), gotBins[i], wantBins[i])
		}
	}
	if got.total != want.total {
		t.Fatalf("total: got %v, want %v", got.total, want.total)
	}
}

// Below the cap, growLow extends the store geometrically. Without that a
// falling stream reallocates and copies on every observation, which is
// quadratic; the cost is visible only as an allocation count, so that is what
// this asserts.
func TestStore_AFallingStreamBelowTheCapAmortisesItsGrowth(t *testing.T) {
	var s store
	s.add(0, 1)

	k := -1
	// ~1000 adds keeps the whole run under MaxBins, so this measures the
	// growth strategy rather than the short-circuit at the cap.
	allocs := testing.AllocsPerRun(1000, func() {
		s.add(k, 1)
		k--
	})
	if allocs > 0.2 {
		t.Errorf("growing downwards allocated %v times per add; geometric growth is ~0.01, "+
			"one-bucket-at-a-time is 1", allocs)
	}
	if got := len(s.counts); got > 2*(-k) {
		t.Errorf("width %d for %d buckets of data: padding should stay within a factor of two", got, -k)
	}
}

// merge sizes the destination to the source, once. A destination that ends up
// wider than the data it was given is the signature of the pre-size having
// gone to the wrong place and the adds having cleaned up after it.
func TestStore_MergeSizesTheDestinationToTheSource(t *testing.T) {
	var src store
	for k := 0; k < 500; k++ {
		src.add(k, 1)
	}

	var dst store
	dst.merge(&src)

	if got, want := len(dst.counts), 500; got != want {
		t.Errorf("width: got %d, want %d — exactly the source's span", got, want)
	}
	if got, want := dst.offset, 0; got != want {
		t.Errorf("offset: got %d, want %d", got, want)
	}
}

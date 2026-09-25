package sketch

import "math"

// MaxBins is how many buckets one store keeps before it starts collapsing.
//
// 2048 buckets at γ ≈ 1.02 span a ratio of γ^2048 ≈ 10^17 between the smallest
// and largest value a sketch can separate — nanoseconds to centuries. Nothing
// real collapses; the cap exists so a metric fed garbage cannot allocate
// without bound, not because the budget is tight.
const MaxBins = 2048

// store is a contiguous run of bucket counts.
//
// Dense rather than a map, because every operation here walks the buckets in
// index order — quantiles scan, merges zip, encoding emits sorted pairs — and
// a map would mean sorting the keys on each one. Real data is contiguous
// anyway: latencies from one endpoint occupy a narrow band of indices, so the
// slice is nearly full. A sparse metric wastes some zeroes and is bounded by
// MaxBins regardless.
//
// The zero value is an empty store and is ready to use.
type store struct {
	// counts[i] holds bucket (offset + i). Fractional because a sampled
	// statsd metric contributes 1/rate per observation.
	counts []float64
	offset int
	// total is the sum of counts, maintained incrementally: quantiles need it
	// on every call and re-summing 2048 floats would also drift.
	total float64
}

// add puts weight into bucket k, growing or collapsing as needed.
func (s *store) add(k int, weight float64) {
	s.grow(k)
	// grow may have collapsed k itself into the floor bucket, so the index is
	// recomputed rather than remembered.
	i := k - s.offset
	if i < 0 {
		i = 0
	}
	s.counts[i] += weight
	s.total += weight
}

// grow makes bucket k addressable, without putting anything in it. After it
// returns, k is either in range or below the floor that absorbed it.
func (s *store) grow(k int) {
	if len(s.counts) == 0 {
		s.counts = make([]float64, 1, 8)
		s.offset = k
		return
	}
	switch {
	case k < s.offset:
		s.growLow(k)
	case k >= s.offset+len(s.counts):
		s.growHigh(k)
	}
}

// growLow extends the store downwards to cover k, collapsing if that would
// exceed MaxBins.
//
// Collapsing downwards means the new floor absorbs k itself rather than giving
// it a bucket. That is the asymmetry the package is built around: the low end
// of a latency distribution is the part nobody asks about. Only growHigh has
// to fold existing counts, because only it moves the floor upwards.
func (s *store) growLow(k int) {
	if len(s.counts) == MaxBins {
		// Already as wide as it is allowed to be, so the floor cannot move
		// down: k folds into it, which add does when it clamps the index.
		// Falling through would compute a new floor equal to the current one
		// and copy every bucket to the offset it already has — MaxBins of
		// them, once per observation, on a falling stream.
		return
	}
	need := s.offset + len(s.counts) - k // buckets needed to cover [k, top]
	if need <= MaxBins {
		// Grow past k, geometrically, for the reason growHigh keeps spare
		// capacity: a falling stream lands below the floor over and over, and
		// extending to exactly k each time copies the whole store on every
		// observation. A slice cannot hold spare capacity at its *front*, so
		// the headroom here is length rather than capacity — leading empty
		// buckets, which every walk skips and every encoding omits.
		//
		// Capped at MaxBins so the padding can never bring a collapse
		// forward. It could not lose data if it did — both collapses anchor
		// the new floor to the top of the store, which padding does not move
		// — but it would cost memory for buckets nothing asked for.
		want := min(max(need, 2*len(s.counts)), MaxBins)
		newOffset := s.offset + len(s.counts) - want
		grown := make([]float64, want)
		copy(grown[s.offset-newOffset:], s.counts)
		s.counts, s.offset = grown, newOffset
		return
	}
	// Keep the top MaxBins buckets. Nothing already held falls off the bottom,
	// and it cannot: the store is never wider than MaxBins, so the new floor
	// always sits at or below the current offset. The only thing landing below
	// the floor is k itself, which add folds in when it clamps the index.
	newOffset := s.offset + len(s.counts) - MaxBins
	grown := make([]float64, MaxBins)
	copy(grown[s.offset-newOffset:], s.counts)
	s.counts, s.offset = grown, newOffset
}

// growHigh extends the store upwards to cover k, collapsing the low end if
// that would exceed MaxBins.
func (s *store) growHigh(k int) {
	want := k - s.offset + 1
	if want <= MaxBins {
		// Spare capacity, not just the length asked for: a rising stream of
		// values lands in a new top bucket again and again, and growing by one
		// slot each time would copy the whole store every few observations.
		// Length stays exact, so the collapse arithmetic below is unaffected.
		if want <= cap(s.counts) {
			s.counts = s.counts[:want]
			return
		}
		grown := make([]float64, want, min(max(2*want, 8), MaxBins))
		copy(grown, s.counts)
		s.counts = grown
		return
	}
	newOffset := k - MaxBins + 1
	var folded float64
	for i := 0; i < newOffset-s.offset && i < len(s.counts); i++ {
		folded += s.counts[i]
	}
	grown := make([]float64, MaxBins)
	if from := newOffset - s.offset; from < len(s.counts) {
		copy(grown, s.counts[from:])
	}
	grown[0] += folded
	s.counts, s.offset = grown, newOffset
}

// merge adds every bucket of other into s.
//
// The destination's top is sized once, up front. Letting the adds discover it
// reallocates on the way up, which is quadratic in the width of the source —
// and the query path merges hundreds of sketches per request. The bottom needs
// no such help: the adds run ascending, so the lowest bucket is the first one
// they place, and growLow pads geometrically from there.
func (s *store) merge(other *store) {
	hi := -1
	for i, c := range other.counts {
		if c != 0 {
			hi = i
		}
	}
	if hi < 0 {
		return
	}
	s.grow(other.offset + hi)
	for i := 0; i <= hi; i++ {
		if c := other.counts[i]; c != 0 {
			s.add(other.offset+i, c)
		}
	}
}

// forEach calls fn with each non-empty bucket, in ascending index order.
func (s *store) forEach(fn func(k int, count float64)) {
	for i, c := range s.counts {
		if c != 0 {
			fn(s.offset+i, c)
		}
	}
}

// forEachDesc is forEach in descending index order, for walking the negative
// store — whose indices run the opposite way to the values they represent.
func (s *store) forEachDesc(fn func(k int, count float64)) {
	for i := len(s.counts) - 1; i >= 0; i-- {
		if c := s.counts[i]; c != 0 {
			fn(s.offset+i, c)
		}
	}
}

// bins returns the non-empty buckets as sorted (index, count) pairs, for the
// wire encoding.
func (s *store) bins() []Bin {
	out := make([]Bin, 0, len(s.counts))
	s.forEach(func(k int, c float64) { out = append(out, Bin{Index: k, Count: c}) })
	return out
}

// isEmpty reports whether anything has been added.
func (s *store) isEmpty() bool { return s.total == 0 && len(s.counts) == 0 }

// clone returns a deep copy, so a merge cannot write through into its source.
func (s *store) clone() store {
	c := store{offset: s.offset, total: s.total}
	if s.counts != nil {
		c.counts = make([]float64, len(s.counts))
		copy(c.counts, s.counts)
	}
	return c
}

// finite reports whether v is a real number this package can index.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

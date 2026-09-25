package sketch

import (
	"fmt"
	"math"
)

// Bin is one bucket of a sketch: the index and how many observations landed in
// it. It is the unit the wire format and the on-disk encoding both carry, so
// it is exported and deliberately dull.
type Bin struct {
	Index int
	Count float64
}

// PositiveBins returns the non-empty positive buckets, ascending by index.
func (s *Sketch) PositiveBins() []Bin { return s.pos.bins() }

// NegativeBins returns the non-empty negative buckets, ascending by index.
// The index is of the absolute value, so ascending index is *descending*
// value — the encoding keeps them in index order and the decoder does the
// same, so the two never have to agree on which direction is natural.
func (s *Sketch) NegativeBins() []Bin { return s.neg.bins() }

// AddBin restores a bucket directly, for a decoder rebuilding a sketch that
// was encoded elsewhere.
//
// It deliberately does not touch count, sum, min or max: those are carried
// exactly on the wire and restoring them from buckets would replace known
// values with estimates. [Sketch.SetAggregates] sets them.
//
// It returns an error rather than absorbing whatever it is handed, because
// every caller is a decoder holding bytes from another process. A NaN count
// poisons the running total and makes every later quantile comparison false,
// so the sketch answers max for everything — plausible and wrong. An index
// outside ±2^31 overflows the store's width arithmetic and panics on the next
// append; [Sketch.Add] clamps its own indices to that range precisely so the
// store never has to deal with one, and no encoder can emit a wider one. An
// empty bucket is not corruption — a sparse encoding may carry one — and is
// skipped.
func (s *Sketch) AddBin(b Bin, negative bool) error {
	if !finite(b.Count) {
		return fmt.Errorf("%w: count %v", ErrBadBin, b.Count)
	}
	if b.Count <= 0 {
		return nil
	}
	if b.Index < math.MinInt32 || b.Index > math.MaxInt32 {
		return fmt.Errorf("%w: index %d is outside the range Add can produce", ErrBadBin, b.Index)
	}
	if negative {
		s.neg.add(b.Index, b.Count)
		return nil
	}
	s.pos.add(b.Index, b.Count)
	return nil
}

// SetAggregates restores the exact aggregates a decoder read off the wire.
//
// Validated for the same reason [Sketch.AddBin] is: these are the numbers
// Quantile trusts completely. A NaN count makes every rank NaN, and a min
// above its max silently inverts the clamp that is supposed to make estimates
// more accurate. An empty sketch is the one case where min and max are not
// real numbers — they are the ±Inf sentinels a fresh sketch starts with — so
// they are only required to be finite once there is something to describe.
func (s *Sketch) SetAggregates(count, sum, min, max, zeros float64) error {
	if !finite(count) || count < 0 {
		return fmt.Errorf("%w: count %v", ErrBadBin, count)
	}
	if !finite(zeros) || zeros < 0 {
		return fmt.Errorf("%w: zero count %v", ErrBadBin, zeros)
	}
	if !finite(sum) {
		return fmt.Errorf("%w: sum %v", ErrBadBin, sum)
	}
	if count > 0 {
		if !finite(min) || !finite(max) {
			return fmt.Errorf("%w: min %v and max %v must be finite for a non-empty sketch", ErrBadBin, min, max)
		}
		if min > max {
			return fmt.Errorf("%w: min %v is above max %v", ErrBadBin, min, max)
		}
	}
	s.count, s.sum, s.min, s.max, s.zeros = count, sum, min, max, zeros
	return nil
}

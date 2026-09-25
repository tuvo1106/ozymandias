package sketch

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
func (s *Sketch) AddBin(b Bin, negative bool) {
	if b.Count <= 0 {
		return
	}
	if negative {
		s.neg.add(b.Index, b.Count)
		return
	}
	s.pos.add(b.Index, b.Count)
}

// SetAggregates restores the exact aggregates a decoder read off the wire.
func (s *Sketch) SetAggregates(count, sum, min, max, zeros float64) {
	s.count, s.sum, s.min, s.max, s.zeros = count, sum, min, max, zeros
}

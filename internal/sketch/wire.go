package sketch

import (
	"fmt"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// ToWire renders the sketch in the shape wire-protocol §D carries.
//
// γ goes with it because γ is what the bucket indices mean; see [wire.Sketch].
// An empty sketch reports min and max as zero rather than its ±Inf sentinels,
// which have no JSON form — [FromWire] restores the sentinels from the zero
// count, so the round trip is exact.
func (s *Sketch) ToWire() wire.Sketch {
	w := wire.Sketch{
		Gamma:   s.gamma,
		Count:   s.count,
		Sum:     s.sum,
		Zeros:   s.zeros,
		Bins:    toWireBins(s.pos.bins()),
		NegBins: toWireBins(s.neg.bins()),
	}
	if s.count > 0 {
		w.Min, w.Max = s.min, s.max
	}
	return w
}

func toWireBins(bins []Bin) []wire.SketchBin {
	// Never nil: `"bins": null` and `"bins": []` are the same sketch, and
	// only one of them reads as one.
	out := make([]wire.SketchBin, len(bins))
	for i, b := range bins {
		out[i] = wire.SketchBin{Index: b.Index, Count: b.Count}
	}
	return out
}

// FromWire rebuilds a sketch from a §D payload.
//
// The payload is bytes from another process, so every field goes through the
// same validation an operator-facing decoder would apply — [wire.ValidateSketch]
// first, then the package's own entry points, which reject an index or a count
// the store cannot represent. A sketch that comes back from here is as
// trustworthy as one built by [Sketch.Add].
func FromWire(w wire.Sketch) (*Sketch, error) {
	if err := wire.ValidateSketch(&w); err != nil {
		return nil, fmt.Errorf("sketch: %w", err)
	}
	s, err := NewWithGamma(w.Gamma)
	if err != nil {
		return nil, err
	}
	for _, b := range w.Bins {
		if err := s.AddBin(Bin{Index: b.Index, Count: b.Count}, false); err != nil {
			return nil, err
		}
	}
	for _, b := range w.NegBins {
		if err := s.AddBin(Bin{Index: b.Index, Count: b.Count}, true); err != nil {
			return nil, err
		}
	}
	if w.Count == 0 {
		// Nothing was observed, so the aggregates stay at the sentinels a
		// fresh sketch carries. Writing the zeros the wire uses for them
		// would make the next Add compare against 0 and keep it.
		return s, nil
	}
	if err := s.SetAggregates(w.Count, w.Sum, w.Min, w.Max, w.Zeros); err != nil {
		return nil, err
	}
	return s, nil
}

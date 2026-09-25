package sketch

import (
	"errors"
	"fmt"
	"math"
)

// DefaultAlpha is the relative-error bound every sketch in ozymandias uses.
//
// One percent is the number the whole pipeline is specified against: the agent
// sketches with it, the wire format carries the γ it implies, and the M2
// acceptance criterion is a p95 within 1% of exact. It is also about the point
// where a tighter bound stops being free — halving α roughly doubles the
// bucket count for the same value range.
const DefaultAlpha = 0.01

// Errors this package returns. They are values because callers act on them:
// the intake path turns a mismatched γ into a 400 and a NaN into a dropped
// point, and those are different decisions.
var (
	// ErrIncompatible means two sketches were built with different γ, so
	// their buckets describe different value ranges and adding them would be
	// arithmetic on unlike units.
	ErrIncompatible = errors.New("sketch: sketches have different gamma")
	// ErrNotFinite means a NaN or an infinity reached Add. There is no bucket
	// for it and silently dropping it would make count disagree with what the
	// caller believes it sent.
	ErrNotFinite = errors.New("sketch: value is not finite")
	// ErrBadBin means a decoder handed over a bucket or an aggregate no
	// encoder in this project could have produced. Both entry points take
	// bytes from another process, so neither trusts them.
	ErrBadBin = errors.New("sketch: bin is not representable")
	// ErrBadQuantile means q was outside [0, 1].
	ErrBadQuantile = errors.New("sketch: quantile must be in [0, 1]")
)

// Sketch is a DDSketch: a mergeable quantile sketch over one interval.
//
// Not safe for concurrent use. The aggregator owns one per context per bucket
// and hands it off rather than sharing it.
type Sketch struct {
	gamma float64
	// logGamma is 1/ln(γ), cached because indexing is the hot path: one
	// division saved per observation, and the reciprocal is exact enough that
	// the bucket boundary does not move.
	invLogGamma float64

	pos, neg store
	zeros    float64

	// Exact aggregates, kept alongside the buckets. They cost four words and
	// buy exact Count/Sum/Min/Max, which is what makes Quantile(0) and
	// Quantile(1) exact instead of bucket estimates.
	count, sum float64
	min, max   float64
}

// New returns an empty sketch with the given relative-error bound.
//
// alpha must be in (0, 1). Anything outside that is a programming error rather
// than bad input — there is no α that means "no error" and none above 1 that
// means anything at all — so it panics, which [NewDefault] exists to avoid
// having to think about.
func New(alpha float64) *Sketch {
	if !(alpha > 0 && alpha < 1) {
		panic(fmt.Sprintf("sketch: alpha %v is not in (0, 1)", alpha))
	}
	gamma := (1 + alpha) / (1 - alpha)
	if gamma <= 1 {
		// Below roughly 1e-16, (1+a)/(1-a) rounds to exactly 1: log γ is zero,
		// every value maps to the same bucket, and the index arithmetic
		// divides by zero. NewWithGamma rejects such a γ off the wire; an α
		// that derives one is the same programming error as an α outside
		// (0, 1), and is worth saying so rather than returning a sketch that
		// answers 1 for everything.
		panic(fmt.Sprintf("sketch: alpha %v is too small to represent — gamma rounds to 1", alpha))
	}
	return &Sketch{
		gamma:       gamma,
		invLogGamma: 1 / math.Log(gamma),
		min:         math.Inf(1),
		max:         math.Inf(-1),
	}
}

// NewDefault returns an empty sketch at [DefaultAlpha].
func NewDefault() *Sketch { return New(DefaultAlpha) }

// NewWithGamma rebuilds a sketch for a γ that came off the wire, rather than
// from an α this process chose.
//
// The wire carries γ and not α because γ is what the bucket indices actually
// mean: a decoder that recomputed γ from a rounded α would place the same
// index at a slightly different value, and the two sketches would stop being
// mergeable for reasons invisible in the payload.
func NewWithGamma(gamma float64) (*Sketch, error) {
	if !finite(gamma) || gamma <= 1 {
		return nil, fmt.Errorf("sketch: gamma %v must be finite and greater than 1", gamma)
	}
	return &Sketch{
		gamma:       gamma,
		invLogGamma: 1 / math.Log(gamma),
		min:         math.Inf(1),
		max:         math.Inf(-1),
	}, nil
}

// Gamma returns the sketch's bucket growth factor.
func (s *Sketch) Gamma() float64 { return s.gamma }

// Alpha returns the relative-error bound γ implies.
func (s *Sketch) Alpha() float64 { return (s.gamma - 1) / (s.gamma + 1) }

// Add records one observation.
func (s *Sketch) Add(v float64) error { return s.AddWithCount(v, 1) }

// AddWithCount records an observation seen weight times.
//
// Fractional weights are the normal case, not an edge case: a statsd client
// sampling at 0.1 sends one packet to mean ten observations, so the agent adds
// it with weight 10. Counts are float64 throughout for that reason.
func (s *Sketch) AddWithCount(v float64, weight float64) error {
	if !finite(v) {
		return fmt.Errorf("%w: %v", ErrNotFinite, v)
	}
	if !finite(weight) || weight <= 0 {
		return fmt.Errorf("sketch: weight %v must be finite and positive", weight)
	}
	switch {
	case v > 0:
		s.pos.add(s.index(v), weight)
	case v < 0:
		s.neg.add(s.index(-v), weight)
	default:
		s.zeros += weight
	}
	s.count += weight
	s.sum += v * weight
	if v < s.min {
		s.min = v
	}
	if v > s.max {
		s.max = v
	}
	return nil
}

// index maps a positive value to its bucket: k = ceil(log_γ v), so bucket k
// covers (γ^(k-1), γ^k].
//
// Ceil and not floor, and the difference is the whole guarantee: it is what
// makes γ^k the bucket's *upper* bound, so the estimate 2γ^k/(γ+1) sits where
// the relative error is exactly α at both ends of the bucket and less in the
// middle.
func (s *Sketch) index(v float64) int {
	k := math.Ceil(math.Log(v) * s.invLogGamma)
	// A subnormal or an astronomically large value produces an index that no
	// realistic store spans; clamping keeps the int conversion defined and
	// costs only the separability of values nothing measures. The value is
	// still counted, and still in count, sum and min/max.
	if k < math.MinInt32 {
		return math.MinInt32
	}
	if k > math.MaxInt32 {
		return math.MaxInt32
	}
	return int(k)
}

// value returns the estimate for bucket k: 2γ^k/(γ+1), the point in
// (γ^(k-1), γ^k] whose relative error is α at both ends.
func (s *Sketch) value(k int) float64 {
	return 2 * math.Pow(s.gamma, float64(k)) / (s.gamma + 1)
}

// Count returns the number of observations, exactly.
func (s *Sketch) Count() float64 { return s.count }

// Sum returns the sum of the observations, exactly.
func (s *Sketch) Sum() float64 { return s.sum }

// Min returns the smallest observation, exactly, or +Inf if there are none.
func (s *Sketch) Min() float64 { return s.min }

// Max returns the largest observation, exactly, or -Inf if there are none.
func (s *Sketch) Max() float64 { return s.max }

// ZeroCount returns how many observations were exactly zero.
//
// Zero needs its own counter because log_γ 0 is undefined: there is no bucket
// for it, and folding it into the lowest positive bucket would report zero as
// a small positive number.
func (s *Sketch) ZeroCount() float64 { return s.zeros }

// Merge adds other into s. Both must have the same γ.
//
// This is the operation the whole design exists for: a query merges every
// sketch in a group — many hosts, many intervals, in whatever order the
// storage returns them — and the answer is the same as if one sketch had seen
// every observation. Addition is associative and commutative, so the merge is
// too.
func (s *Sketch) Merge(other *Sketch) error {
	if other == nil {
		return nil
	}
	if s.gamma != other.gamma {
		return fmt.Errorf("%w: %v and %v", ErrIncompatible, s.gamma, other.gamma)
	}
	s.pos.merge(&other.pos)
	s.neg.merge(&other.neg)
	s.zeros += other.zeros
	s.count += other.count
	s.sum += other.sum
	if other.min < s.min {
		s.min = other.min
	}
	if other.max > s.max {
		s.max = other.max
	}
	return nil
}

// Clone returns a deep copy.
func (s *Sketch) Clone() *Sketch {
	c := *s
	c.pos = s.pos.clone()
	c.neg = s.neg.clone()
	return &c
}

// Quantile returns the value at q, within a relative error of [Sketch.Alpha]
// — except at the ends, which are exact.
//
// It returns NaN for an empty sketch, because there is no value to be within
// α of and any number would be a lie. Callers render that as "no data".
//
// The rank convention is q*(count-1), the same one NumPy's linear
// interpolation and DataDog's implementation use: q=0 is the minimum and q=1
// the maximum, with no off-by-one at either end.
func (s *Sketch) Quantile(q float64) (float64, error) {
	if !(q >= 0 && q <= 1) {
		return math.NaN(), fmt.Errorf("%w: %v", ErrBadQuantile, q)
	}
	if s.count == 0 {
		return math.NaN(), nil
	}
	// The extremes are tracked exactly, so answer them exactly rather than
	// from a bucket. This is also what makes a single-observation sketch
	// return that observation for every q.
	if q == 0 {
		return s.min, nil
	}
	if q == 1 {
		return s.max, nil
	}

	rank := q * (s.count - 1)
	// A sampled metric reports 1/rate per observation, so a sketch can hold a
	// total count below 1 — and then q*(count-1) is negative. Rank zero is the
	// right answer there: the first value in ascending order. Left negative it
	// makes the *first* `seen > rank` comparison true whatever it is testing,
	// including the zeros branch below on a sketch holding no zero at all.
	if rank < 0 {
		rank = 0
	}

	// Negatives first, from most negative to least: the negative store indexes
	// |v|, so a *larger* index is a *smaller* value and the walk is
	// descending.
	var seen float64
	var found float64
	var ok bool
	s.neg.forEachDesc(func(k int, c float64) {
		if ok {
			return
		}
		if seen += c; seen > rank {
			found, ok = -s.value(k), true
		}
	})
	if ok {
		return clampToRange(found, s.min, s.max), nil
	}
	if seen += s.zeros; seen > rank {
		return 0, nil
	}
	s.pos.forEach(func(k int, c float64) {
		if ok {
			return
		}
		if seen += c; seen > rank {
			found, ok = s.value(k), true
		}
	})
	if ok {
		return clampToRange(found, s.min, s.max), nil
	}
	// Floating-point accumulation can leave the running total a hair under
	// rank at the very top. The answer there is the largest value, which is
	// tracked exactly.
	return s.max, nil
}

// clampToRange keeps a bucket estimate inside the observed range.
//
// A bucket's estimate is the midpoint of a range the observations only partly
// fill, so it can land just outside the real minimum or maximum — p99 of a
// sketch whose largest value is 96ms should not be reported as 97ms when 96 is
// known exactly. The clamp can only make the answer more accurate: min and max
// are real observations, so moving an estimate onto one moves it towards the
// truth.
func clampToRange(v, min, max float64) float64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

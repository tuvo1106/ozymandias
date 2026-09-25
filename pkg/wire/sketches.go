package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// KindDistribution is the type recorded for a metric that arrives on
// /v1/sketches (§D).
//
// It never appears in a /v1/series body — the endpoint says what the payload
// is, so a `type` field would only be a second, disagreeing source of truth —
// but it is what the metadata registry stores, and what tells the query layer
// that this metric answers percentiles rather than avg and sum.
const KindDistribution Kind = "distribution"

// Suffixes of the ordinary series ozyd writes beside every sketch (§D).
//
// The four aggregates a sketch carries are exact, so they are worth storing
// as plain series: `avg:latency.sum / avg:latency.count` is a cheap, honest
// average, and no percentile machinery has to run to draw it. `<metric>.count`
// has a second job — it is how a percentile query finds which series exist,
// since it carries the same tags and is written in the same request as the
// sketch. See docs/adr/0015-sketch-storage-and-identity.md.
const (
	SuffixCount = ".count"
	SuffixSum   = ".sum"
	SuffixMin   = ".min"
	SuffixMax   = ".max"
)

// Limits on a /v1/sketches body (§D).
const (
	// MaxSketchesPerRequest bounds one body; the forwarder splits.
	MaxSketchesPerRequest = 5000
	// MaxBinsPerSketch bounds the buckets one sketch may carry, per sign.
	// It is the sketch implementation's own cap: a payload wider than that
	// was not produced by any agent, and decoding it would allocate for
	// buckets the store would immediately collapse. (internal/sketch pins
	// the two together in a test.)
	MaxBinsPerSketch = 2048
	// MaxPointsPerSketchSeries bounds the buckets one series may carry in one
	// request. An agent flushing every 10s sends one, and catches up after an
	// outage a few dozen at a time.
	MaxPointsPerSketchSeries = 1000
)

// SketchBin is one bucket: the index k and how much landed in it. Bucket k
// covers (γ^(k-1), γ^k]. On the wire it is the pair [k, count].
type SketchBin struct {
	Index int
	Count float64
}

// MarshalJSON writes [index, count].
func (b SketchBin) MarshalJSON() ([]byte, error) {
	if math.IsNaN(b.Count) || math.IsInf(b.Count, 0) {
		return nil, fmt.Errorf("bin count %v is not finite", b.Count)
	}
	out := strconv.AppendInt([]byte{'['}, int64(b.Index), 10)
	out = append(out, ',')
	out = strconv.AppendFloat(out, b.Count, 'g', -1, 64)
	return append(out, ']'), nil
}

// UnmarshalJSON reads [index, count]. The index must be an integer: a bucket
// at 3.5 is not a bucket, and reading it as 3 would silently move counts.
func (b *SketchBin) UnmarshalJSON(data []byte) error {
	var pair []json.Number
	if err := json.Unmarshal(data, &pair); err != nil {
		return fmt.Errorf("bin: want [index, count]: %w", err)
	}
	if len(pair) != 2 {
		return fmt.Errorf("bin: want [index, count], got %d elements", len(pair))
	}
	k, err := pair[0].Int64()
	if err != nil {
		return fmt.Errorf("bin index %q: want an integer", pair[0])
	}
	if k < math.MinInt32 || k > math.MaxInt32 {
		return fmt.Errorf("bin index %d is outside ±2^31", k)
	}
	c, err := strconv.ParseFloat(pair[1].String(), 64)
	if err != nil {
		return fmt.Errorf("bin count %q: %w", pair[1], errors.Unwrap(err))
	}
	b.Index, b.Count = int(k), c
	return nil
}

// Sketch is one DDSketch as it crosses the wire (§D).
//
// γ travels with every sketch rather than being deployment configuration,
// because γ is what the bucket indices *mean*. Two agents configured with
// different relative accuracies produce indices that look alike and are not,
// and a receiver that assumed its own γ would merge them into a confident
// wrong answer. Carrying it makes the mismatch detectable.
//
// Count, Sum, Min and Max are exact — they are accumulated, not estimated —
// and are what the `.count/.sum/.min/.max` series are written from.
type Sketch struct {
	Gamma float64 `json:"gamma"`
	Count float64 `json:"count"`
	Sum   float64 `json:"sum"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Zeros float64 `json:"zero_count"`
	// Bins are the positive buckets, ascending by index; NegBins the
	// negative ones, indexed by absolute value and also ascending.
	Bins    []SketchBin `json:"bins"`
	NegBins []SketchBin `json:"neg_bins"`
}

// SketchPoint is one sketch at one bucket start (unix seconds).
type SketchPoint struct {
	Timestamp int64  `json:"ts"`
	Sketch    Sketch `json:"sketch"`
}

// SketchSeries is one metric context and its sketches, as the agent flushes
// them (§D).
type SketchSeries struct {
	Metric string   `json:"metric"`
	Tags   []string `json:"tags"`
	// Interval is the bucket width in seconds, always positive: a sketch
	// describes a window, never an instant.
	Interval int64         `json:"interval"`
	Points   []SketchPoint `json:"points"`
}

// SketchesPayload is the body of POST /v1/sketches.
type SketchesPayload struct {
	Sketches []SketchSeries `json:"sketches"`
}

// DecodeSketches parses a /v1/sketches body and validates each series on its
// own (§D), exactly as [DecodeSeries] does for /v1/series: the valid ones come
// back with canonical tags, the invalid ones as rejections, and the error is
// reserved for a body that is not a sketch payload at all.
func DecodeSketches(body []byte, opts DecodeOptions) ([]SketchSeries, []Rejection, error) {
	var raw struct {
		Sketches []json.RawMessage `json:"sketches"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, nil, fmt.Errorf("body is not a sketch payload: %w", err)
	}
	if raw.Sketches == nil {
		return nil, nil, errors.New(`body has no "sketches" array`)
	}
	if len(raw.Sketches) > MaxSketchesPerRequest {
		return nil, nil, fmt.Errorf("%d sketch series in one request; the limit is %d", len(raw.Sketches), MaxSketchesPerRequest)
	}
	valid := make([]SketchSeries, 0, len(raw.Sketches))
	var rejects []Rejection
	for i, item := range raw.Sketches {
		var s SketchSeries
		if err := json.Unmarshal(item, &s); err != nil {
			rejects = append(rejects, Rejection{Index: i, Metric: s.Metric, Reason: err.Error()})
			continue
		}
		if err := ValidateSketchSeries(&s, opts); err != nil {
			rejects = append(rejects, Rejection{Index: i, Metric: s.Metric, Reason: err.Error()})
			continue
		}
		s.Tags = CanonicalTags(s.Tags)
		valid = append(valid, s)
	}
	return valid, rejects, nil
}

// ValidateSketchSeries checks one series against §D. It does not modify s.
//
// The unit of rejection is the series, for the same reason it is on /v1/series:
// a rule a third-party sender can predict beats one that salvages a little
// more of a broken payload.
func ValidateSketchSeries(s *SketchSeries, opts DecodeOptions) error {
	if !ValidMetricName(s.Metric) {
		return fmt.Errorf("invalid metric name (want ^[a-zA-Z][a-zA-Z0-9_.]{0,%d}$)", MaxMetricNameLen-1)
	}
	if s.Interval <= 0 {
		return fmt.Errorf("a sketch series needs a positive interval, got %d", s.Interval)
	}
	if len(s.Tags) > MaxTagsPerPoint {
		return fmt.Errorf("%d tags; the limit is %d", len(s.Tags), MaxTagsPerPoint)
	}
	for _, t := range s.Tags {
		if !ValidTag(t) {
			return fmt.Errorf("invalid tag %q", t)
		}
	}
	switch {
	case len(s.Points) == 0:
		return errors.New("no points")
	case len(s.Points) > MaxPointsPerSketchSeries:
		return fmt.Errorf("%d points; the limit is %d", len(s.Points), MaxPointsPerSketchSeries)
	}
	latest := opts.Now.Add(MaxFutureSkew).Unix()
	for _, p := range s.Points {
		if p.Timestamp <= 0 {
			return fmt.Errorf("point timestamp %d is not a positive unix time", p.Timestamp)
		}
		if p.Timestamp > latest {
			return fmt.Errorf("point at %d is more than %s in the future", p.Timestamp, MaxFutureSkew)
		}
		if opts.MaxAge > 0 && p.Timestamp < opts.Now.Add(-opts.MaxAge).Unix() {
			return fmt.Errorf("point at %d is older than the accepted window (%s)", p.Timestamp, opts.MaxAge)
		}
		if err := ValidateSketch(&p.Sketch); err != nil {
			return fmt.Errorf("point at %d: %w", p.Timestamp, err)
		}
	}
	return nil
}

// ValidateSketch checks one sketch's internal consistency (§D).
func ValidateSketch(s *Sketch) error {
	if !finite(s.Gamma) || s.Gamma <= 1 {
		return fmt.Errorf("gamma %v must be finite and greater than 1", s.Gamma)
	}
	if !finite(s.Count) || s.Count < 0 {
		return fmt.Errorf("count %v must be finite and not negative", s.Count)
	}
	if !finite(s.Zeros) || s.Zeros < 0 {
		return fmt.Errorf("zero_count %v must be finite and not negative", s.Zeros)
	}
	if !finite(s.Sum) {
		return fmt.Errorf("sum %v is not finite", s.Sum)
	}
	if s.Count > 0 {
		if !finite(s.Min) || !finite(s.Max) {
			return fmt.Errorf("min %v and max %v must be finite when count is %v", s.Min, s.Max, s.Count)
		}
		if s.Min > s.Max {
			return fmt.Errorf("min %v is above max %v", s.Min, s.Max)
		}
	}
	if err := validateBins(s.Bins, "bins"); err != nil {
		return err
	}
	if err := validateBins(s.NegBins, "neg_bins"); err != nil {
		return err
	}
	// The buckets and the exact count have to describe the same observations.
	// A quantile resolves a rank out of count and then walks the buckets to
	// find it, so if the two disagree the walk runs off the end and the
	// sketch answers max for everything above the buckets' own total — an
	// answer with no error bound at all, and no sign that anything is wrong.
	// The tolerance is for re-summation order, not for disagreement.
	var binned float64
	for _, b := range s.Bins {
		binned += b.Count
	}
	for _, b := range s.NegBins {
		binned += b.Count
	}
	binned += s.Zeros
	if math.Abs(binned-s.Count) > 1e-6*s.Count+1e-9 {
		return fmt.Errorf("buckets hold %v observations but count says %v", binned, s.Count)
	}
	return nil
}

// validateBins checks one store's buckets: bounded in number, ascending by
// index with no duplicates, and carrying a usable count.
//
// The ordering requirement is not fussiness. A decoder that accepted any
// order would have to sort, and a duplicate index has two readings — replace
// or add — that give different percentiles from the same bytes.
func validateBins(bins []SketchBin, field string) error {
	if len(bins) > MaxBinsPerSketch {
		return fmt.Errorf("%s: %d buckets; the limit is %d", field, len(bins), MaxBinsPerSketch)
	}
	for i, b := range bins {
		if !finite(b.Count) || b.Count <= 0 {
			return fmt.Errorf("%s[%d]: count %v must be finite and positive", field, i, b.Count)
		}
		if b.Index < math.MinInt32 || b.Index > math.MaxInt32 {
			return fmt.Errorf("%s[%d]: index %d is outside ±2^31", field, i, b.Index)
		}
		if i > 0 && b.Index <= bins[i-1].Index {
			return fmt.Errorf("%s[%d]: index %d is not above the previous %d; buckets must ascend and be unique",
				field, i, b.Index, bins[i-1].Index)
		}
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

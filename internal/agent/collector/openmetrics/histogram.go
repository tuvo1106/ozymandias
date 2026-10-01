package openmetrics

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/tuvo1106/ozymandias/internal/sketch"
)

// Bucket is one histogram bucket: observations ≤ UpperBound.
//
// Count is cumulative in what [Family.Histograms] returns — the exposition
// format's own convention, where each bucket counts everything at or below
// its bound and the +Inf bucket equals the total — and per-bucket (the
// observations in (previous bound, UpperBound]) in what [BucketDeltas]
// returns. One type for both because they are the same shape and every
// function says which it takes.
type Bucket struct {
	UpperBound float64
	Count      float64
}

// Histogram is one series of a histogram family: the samples that share a
// label set once `le` is set aside.
type Histogram struct {
	// Labels are the series' labels without `le`, in the order the first
	// sample wrote them.
	Labels []Label
	// Buckets are cumulative, in ascending UpperBound, ending with +Inf.
	Buckets []Bucket
	// Sum and Count are the `_sum` and `_count` samples, when present.
	Sum, Count       float64
	HasSum, HasCount bool
}

// Histograms groups a histogram (or gaugehistogram) family's samples into
// one Histogram per label set.
//
// A series with no +Inf bucket gets one: the format requires it, but a
// missing one is easy to reconstruct — it is `_count` when that is present,
// and otherwise the largest cumulative count seen, the only lower bound on
// the total there is. `_created` samples are ignored; they say when the
// series started, which [BucketDeltas] does not need because it detects a
// restart from the counts themselves.
func (f *Family) Histograms() ([]Histogram, error) {
	var bucketSfx, sumSfx, countSfx string
	switch f.Type {
	case TypeHistogram:
		bucketSfx, sumSfx, countSfx = "_bucket", "_sum", "_count"
	case TypeGaugeHistogram:
		bucketSfx, sumSfx, countSfx = "_gbucket", "_gsum", "_gcount"
	default:
		return nil, fmt.Errorf("openmetrics: %s is a %s, not a histogram", f.Name, f.Type)
	}
	var out []*Histogram
	byKey := map[string]*Histogram{}
	get := func(labels []Label) *Histogram {
		key := LabelKey(labels, "le")
		h := byKey[key]
		if h == nil {
			h = &Histogram{}
			for _, l := range labels {
				if l.Name != "le" {
					h.Labels = append(h.Labels, l)
				}
			}
			byKey[key] = h
			out = append(out, h)
		}
		return h
	}
	for i := range f.Samples {
		s := &f.Samples[i]
		switch s.Name {
		case f.Name + bucketSfx:
			le, ok := s.Label("le")
			if !ok {
				return nil, fmt.Errorf("openmetrics: %s: a bucket without an le label", s.Name)
			}
			ub, err := strconv.ParseFloat(le, 64)
			if err != nil || math.IsNaN(ub) {
				return nil, fmt.Errorf("openmetrics: %s: le=%q is not a number", s.Name, le)
			}
			h := get(s.Labels)
			h.Buckets = append(h.Buckets, Bucket{UpperBound: ub, Count: s.Value})
		case f.Name + sumSfx:
			h := get(s.Labels)
			h.Sum, h.HasSum = s.Value, true
		case f.Name + countSfx:
			h := get(s.Labels)
			h.Count, h.HasCount = s.Value, true
		}
	}
	res := make([]Histogram, 0, len(out))
	for _, h := range out {
		sort.Slice(h.Buckets, func(i, j int) bool { return h.Buckets[i].UpperBound < h.Buckets[j].UpperBound })
		// Duplicates are found once sorted, where they are neighbours:
		// O(n log n). Checking each bucket against the ones before it as
		// it arrived was O(n²) per series, and a target may serve as many
		// buckets as the sample limit allows — seconds of CPU after the
		// scrape's timeout has passed, since parsing is not cancellable.
		for i := 1; i < len(h.Buckets); i++ {
			if h.Buckets[i].UpperBound == h.Buckets[i-1].UpperBound {
				return nil, fmt.Errorf("openmetrics: %s%s: two buckets with le=%q",
					f.Name, bucketSfx, strconv.FormatFloat(h.Buckets[i].UpperBound, 'g', -1, 64))
			}
		}
		if n := len(h.Buckets); n > 0 && !math.IsInf(h.Buckets[n-1].UpperBound, 1) {
			total := h.Count
			if !h.HasCount {
				for _, b := range h.Buckets {
					total = math.Max(total, b.Count)
				}
			}
			h.Buckets = append(h.Buckets, Bucket{UpperBound: math.Inf(1), Count: total})
		}
		res = append(res, *h)
	}
	return res, nil
}

// LabelKey is a label set's identity, with the label named skip left out
// (pass "" to keep them all): names sorted, so `{a="1",b="2"}` and
// `{b="2",a="1"}` are one series. The one definition of "same series" for
// the parser's histogram grouping and for the openmetrics check's state,
// which must agree.
//
// Each name and value is written with its length in front rather than
// between separator bytes: the parser does not require valid UTF-8, so a
// value may contain any byte, and with separators `{a="x<sep>b<sep>c"}`
// could read as `{a="x",b="c"}`. A length prefix cannot be forged.
func LabelKey(labels []Label, skip string) string {
	ls := make([]Label, 0, len(labels))
	for _, l := range labels {
		if l.Name != skip {
			ls = append(ls, l)
		}
	}
	slices.SortFunc(ls, func(a, b Label) int {
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Value, b.Value)
	})
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(strconv.Itoa(len(l.Name)))
		b.WriteByte(':')
		b.WriteString(l.Name)
		b.WriteString(strconv.Itoa(len(l.Value)))
		b.WriteByte(':')
		b.WriteString(l.Value)
	}
	return b.String()
}

// BucketDeltas turns two consecutive scrapes of one histogram's cumulative
// buckets into what happened between them: the observations that landed in
// each bucket, per bucket (not cumulative), in cur's order.
//
// A target that restarted between scrapes starts its counters again from
// zero, so the difference would be negative — or, worse, positive but small,
// if it has already recounted past part of what it had. The rule is the one
// Prometheus's rate() uses: if any cumulative count went down, the process
// restarted, everything in cur happened since, and cur itself is the delta
// (reset is then true). A change in the bucket layout (a redeploy with new
// boundaries) is treated the same way, because the old counts cannot be
// subtracted from buckets that no longer mean the same thing.
//
// With no previous scrape (prev nil) there is no delta — how much of cur
// happened in the last interval and how much before is unknowable — so it
// returns nil, false; the caller should skip that scrape.
//
// Per-bucket counts are never negative. Exporters read their buckets one at
// a time without a lock, so a scrape can catch cumulative counts that dip
// between adjacent buckets; each bucket's count is taken against the running
// maximum below it, which keeps the total equal to the largest cumulative
// count rather than inventing observations.
func BucketDeltas(prev, cur []Bucket) (deltas []Bucket, reset bool) {
	if prev == nil {
		return nil, false
	}
	cum := make([]Bucket, len(cur))
	copy(cum, cur)
	if len(prev) != len(cur) {
		reset = true
	}
	for i := 0; !reset && i < len(cur); i++ {
		if prev[i].UpperBound != cur[i].UpperBound || cur[i].Count < prev[i].Count {
			reset = true
		}
	}
	if !reset {
		for i := range cum {
			cum[i].Count -= prev[i].Count
		}
	}
	return decumulate(cum), reset
}

// decumulate turns cumulative counts into per-bucket ones, against the
// running maximum (see [BucketDeltas]).
func decumulate(cum []Bucket) []Bucket {
	out := make([]Bucket, len(cum))
	running := 0.0
	for i, b := range cum {
		next := math.Max(running, b.Count)
		out[i] = Bucket{UpperBound: b.UpperBound, Count: next - running}
		running = next
	}
	return out
}

// Subdivisions is how many equal-width points ToSketch spreads one bucket's
// count over.
const Subdivisions = 16

// ErrNoFiniteBucket is returned by ToSketch when observations landed only in
// the +Inf bucket of a histogram with no finite bound: there is no value at
// all to put them at.
var ErrNoFiniteBucket = errors.New("openmetrics: observations only in +Inf, with no finite bucket to place them at")

// ToSketch adds per-bucket counts (from [BucketDeltas]) to s, so that a
// Prometheus histogram can be queried with the same p50…p99 aggregators as a
// native distribution.
//
// A bucket only says that n observations fell somewhere in (lower, upper];
// where is lost. This spreads them uniformly: n/[Subdivisions] at each of
// the Subdivisions midpoints of the interval. Uniform because it is exactly
// the assumption Prometheus's histogram_quantile() makes when it
// interpolates linearly inside a bucket, so the two paths answer the same
// question the same way and differ only by the sketch's own error — which
// is the comparison M3's acceptance criteria ask for. Midpoints and not one
// point per bucket because a single point would put every quantile inside a
// busy bucket at the same value.
//
// The error is bounded by the bucket, not by the sketch: a quantile's true
// value lies in the bucket where its rank falls, and the estimate is within
// that bucket's width (plus width/(2·Subdivisions) for the spreading, plus
// the sketch's relative error α). Coarse buckets mean coarse quantiles; no
// interpolation recovers what the exporter never recorded.
//
// The bounds without a neighbour, following histogram_quantile():
//
//   - The first bucket's lower bound is 0 when its upper bound is positive
//     (latencies and sizes are not negative, and the format has no lower
//     bound to say otherwise). When its upper bound is ≤ 0 there is no
//     sensible lower bound at all, so its count is placed at the upper bound.
//   - The +Inf bucket's count is placed at the largest finite bound: the
//     only thing known is that those observations exceeded it, and claiming
//     any particular larger value would be invented. A p99 that lands there
//     reads as "at least this".
//
// Negative bounds need nothing special beyond that: the interval between two
// negative bounds (or one straddling zero) is spread like any other, and the
// sketch keeps negative values in a store of their own.
func ToSketch(deltas []Bucket, s *sketch.Sketch) error {
	lower := math.Inf(-1)
	for i, b := range deltas {
		if math.IsNaN(b.UpperBound) || math.IsNaN(b.Count) || math.IsInf(b.Count, 0) || b.Count < 0 {
			return fmt.Errorf("openmetrics: bucket %d (le=%v, count=%v) is not usable", i, b.UpperBound, b.Count)
		}
		if i > 0 && b.UpperBound <= deltas[i-1].UpperBound {
			return fmt.Errorf("openmetrics: buckets are not in ascending order at le=%v", b.UpperBound)
		}
		if b.Count > 0 {
			if err := addBucket(s, lower, b.UpperBound, b.Count, i == 0); err != nil {
				return err
			}
		}
		lower = b.UpperBound
	}
	return nil
}

func addBucket(s *sketch.Sketch, lower, upper, n float64, first bool) error {
	switch {
	case math.IsInf(upper, 1):
		if math.IsInf(lower, -1) {
			return ErrNoFiniteBucket
		}
		return s.AddWithCount(lower, n)
	case first && upper <= 0:
		return s.AddWithCount(upper, n)
	case first:
		lower = 0
	}
	w := (upper - lower) / Subdivisions
	for k := range Subdivisions {
		if err := s.AddWithCount(lower+(float64(k)+0.5)*w, n/Subdivisions); err != nil {
			return err
		}
	}
	return nil
}

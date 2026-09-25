package simple

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Agg is the across-series aggregator.
type Agg string

// Aggregators.
const (
	Avg Agg = "avg"
	Sum Agg = "sum"
	Min Agg = "min"
	Max Agg = "max"
)

// MaxBuckets bounds the points per series a query may ask for. A 1-day
// range at a 1-second interval would be 86,400 points per series — more than
// any chart can draw, and a cheap way to make the server do a lot of work.
const MaxBuckets = 10_000

// MaxRange bounds the span a query may cover, in seconds. MaxBuckets alone
// does not: it limits the *output*, and a coarse enough interval satisfies it
// over any range at all — while the store still has to read every sample in
// the range to fill those buckets. Without this, a ten-bucket request could
// ask for a scan of every block on disk. A year is far beyond any plausible
// retention (the default is 15 days).
const MaxRange = 366 * 24 * 60 * 60

// maxTime is the newest accepted timestamp, 9999-12-31T23:59:59Z. Unix
// seconds are unbounded but the bucket arithmetic is not: floorTo multiplies
// back out, and Run scales to milliseconds, both of which wrap silently on
// extreme input. Validate rejects anything outside [0, maxTime] so the rest
// of this package can do that arithmetic without thinking about overflow.
const maxTime = 253402300799

// Request is a structured metric query.
type Request struct {
	Metric  string
	Filters []tsdb.Matcher
	By      []string
	Agg     Agg
	// From and To are unix seconds; the range is [From, To].
	From, To int64
	// Interval is the bucket width in seconds; 0 picks DefaultInterval.
	Interval int64
	// Kind is the metric's type, which decides the time aggregation.
	Kind wire.Kind
}

// Result is a query's answer.
type Result struct {
	From     int64    `json:"from"`
	To       int64    `json:"to"`
	Interval int64    `json:"interval"`
	Series   []Series `json:"series"`
}

// Series is one group's aggregated line.
type Series struct {
	Metric string `json:"metric"`
	// Tags holds the group-by keys and their values; keys a group's series
	// don't have are absent.
	Tags   map[string]string `json:"tags"`
	Points []Point           `json:"points"`
}

// Point is [unix_ms, value]; an empty bucket is NaN in Go and null in JSON,
// so a chart draws a gap rather than a misleading zero.
type Point struct {
	T int64
	V float64
}

// MarshalJSON writes [t, v] or [t, null].
func (p Point) MarshalJSON() ([]byte, error) {
	b := strconv.AppendInt([]byte{'['}, p.T, 10)
	b = append(b, ',')
	if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
		b = append(b, "null"...)
	} else {
		b = strconv.AppendFloat(b, p.V, 'g', -1, 64)
	}
	return append(b, ']'), nil
}

// UnmarshalJSON reads [t, v|null]; null becomes NaN.
func (p *Point) UnmarshalJSON(data []byte) error {
	var pair [2]*float64
	if err := json.Unmarshal(data, &pair); err != nil || pair[0] == nil {
		return fmt.Errorf("point: want [ms, value|null]: %s", data)
	}
	p.T, p.V = int64(*pair[0]), math.NaN()
	if pair[1] != nil {
		p.V = *pair[1]
	}
	return nil
}

// DefaultInterval aims for about 300 points across the range — enough for a
// smooth chart — in whole multiples of the agent's 10-second flush, since a
// bucket narrower than the data's own resolution would just be half empty.
func DefaultInterval(from, to int64) int64 {
	iv := (to - from + 299) / 300
	iv = (iv + 9) / 10 * 10
	return max(iv, 10)
}

// Validate checks a request and fills its defaults (Agg, Interval).
func (r *Request) Validate() error {
	var errs []error
	if !wire.ValidMetricName(r.Metric) {
		errs = append(errs, fmt.Errorf("metric %q is not a valid metric name", r.Metric))
	}
	if r.To <= r.From {
		errs = append(errs, fmt.Errorf("to (%d) must be after from (%d)", r.To, r.From))
	}
	// Checked before anything computes with them; see maxTime.
	if r.From < 0 || r.To > maxTime {
		errs = append(errs, fmt.Errorf("from (%d) and to (%d) must be unix seconds within [0, %d]", r.From, r.To, maxTime))
	}
	// The default is applied before the checks below, not as one of them: an
	// omitted agg and an explicit `agg=avg` are the same request, and a guard
	// that only one of them reaches answers the same question two ways. The
	// omitted one is what the UI sends first.
	if r.Agg == "" {
		r.Agg = Avg
	}
	switch {
	case r.Agg == Avg, r.Agg == Sum, r.Agg == Min, r.Agg == Max:
		if r.Kind == wire.KindDistribution {
			errs = append(errs, fmt.Errorf(
				"%s is a distribution: use p50, p75, p90, p95 or p99, or query %s%s, %s%s, %s%s or %s%s",
				r.Metric, r.Metric, wire.SuffixCount, r.Metric, wire.SuffixSum,
				r.Metric, wire.SuffixMin, r.Metric, wire.SuffixMax))
		}
	case isPercentile(r.Agg):
		if r.Kind != "" && r.Kind != wire.KindDistribution {
			errs = append(errs, fmt.Errorf("%s is a %s, not a distribution, so it has no percentiles", r.Metric, r.Kind))
		}
	default:
		errs = append(errs, fmt.Errorf("agg %q: want avg, sum, min, max, or p50, p75, p90, p95 or p99", r.Agg))
	}
	if r.Interval < 0 {
		errs = append(errs, fmt.Errorf("interval %d must be positive", r.Interval))
	}
	for _, k := range r.By {
		if k == "" || strings.ContainsAny(k, ":,") {
			errs = append(errs, fmt.Errorf("group-by key %q is not a tag key", k))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	// From here on 0 <= From < To <= maxTime, so none of this overflows.
	if r.Interval == 0 {
		r.Interval = DefaultInterval(r.From, r.To)
	}
	// Buckets first: it is the limit a caller hits by asking for too fine a
	// resolution, and its message names the fix. MaxRange catches what is
	// left — a range so wide that even a coarse interval satisfies it.
	if n := (r.To-floorTo(r.From, r.Interval))/r.Interval + 1; n > MaxBuckets {
		return fmt.Errorf("%d buckets at interval %ds; the limit is %d — use a larger interval or a shorter range", n, r.Interval, MaxBuckets)
	}
	if r.To-r.From > MaxRange {
		return fmt.Errorf("range of %ds; the limit is %ds — query a shorter window", r.To-r.From, MaxRange)
	}
	return nil
}

// Run evaluates req against store, reading sketches for the percentile
// aggregators. sketches may be nil on a deployment that has no sketch store;
// a percentile query then says so rather than answering from nothing.
func Run(ctx context.Context, store tsdb.MetricStore, sketches SketchReader, req Request) (Result, error) {
	if err := req.Validate(); err != nil {
		return Result{}, err
	}
	if q, ok := quantileOf(req.Agg); ok {
		return runPercentile(ctx, store, sketches, req, q)
	}
	first := floorTo(req.From, req.Interval)
	n := int((req.To-first)/req.Interval) + 1
	res := Result{From: req.From, To: req.To, Interval: req.Interval, Series: []Series{}}

	set, err := store.Select(ctx, tsdb.Selector{Metric: req.Metric, Matchers: req.Filters}, first*1000, req.To*1000)
	if err != nil {
		return Result{}, err
	}
	defer set.Close()

	sumOverTime := req.Kind == wire.KindCount || req.Kind == wire.KindRate
	groups := map[string]*group{}
	for set.Next() {
		ref := set.Series()
		vals := bucketize(set.Iterator(), first, req.Interval, n, sumOverTime)
		if vals == nil {
			continue
		}
		key, tags := groupKey(ref, req.By)
		g := groups[key]
		if g == nil {
			g = &group{tags: tags, acc: newAccumulator(req.Agg, n)}
			groups[key] = g
		}
		g.acc.add(vals)
	}
	if err := set.Err(); err != nil {
		return Result{}, err
	}

	for _, g := range groups {
		pts := make([]Point, n)
		for i := range pts {
			pts[i] = Point{T: (first + int64(i)*req.Interval) * 1000, V: g.acc.value(i)}
		}
		res.Series = append(res.Series, Series{Metric: req.Metric, Tags: g.tags, Points: pts})
	}
	sortSeries(res.Series)
	return res, nil
}

// sortSeries orders a result deterministically, so a chart's lines and a
// differential test's output do not depend on map iteration.
func sortSeries(series []Series) {
	slices.SortFunc(series, func(a, b Series) int { return strings.Compare(Label(a), Label(b)) })
}

// bucketize reduces one series' samples to n buckets: sum or mean per
// bucket, NaN where the bucket is empty. It returns nil if every bucket is
// empty (possible when the store returns samples outside the range).
func bucketize(it tsdb.SeriesIterator, first, interval int64, n int, sum bool) []float64 {
	totals := make([]float64, n)
	counts := make([]int, n)
	any := false
	for it.Next() {
		s := it.At()
		i := (s.T/1000 - first) / interval
		if i < 0 || i >= int64(n) {
			continue
		}
		totals[i] += s.V
		counts[i]++
		any = true
	}
	if !any {
		return nil
	}
	for i := range totals {
		switch {
		case counts[i] == 0:
			totals[i] = math.NaN()
		case !sum:
			totals[i] /= float64(counts[i])
		}
	}
	return totals
}

type group struct {
	tags map[string]string
	acc  *accumulator
}

func groupKey(ref tsdb.SeriesRef, by []string) (string, map[string]string) {
	tags := map[string]string{}
	var b strings.Builder
	for _, k := range by {
		b.WriteString(k)
		if v, ok := ref.Get(k); ok {
			tags[k] = v
			b.WriteByte('=')
			b.WriteString(v)
		}
		b.WriteByte(0)
	}
	return b.String(), tags
}

// accumulator folds series into one line per group, bucket by bucket,
// ignoring empty (NaN) buckets. A bucket where every series is empty stays
// empty.
type accumulator struct {
	agg    Agg
	vals   []float64
	counts []int
}

func newAccumulator(agg Agg, n int) *accumulator {
	return &accumulator{agg: agg, vals: make([]float64, n), counts: make([]int, n)}
}

func (a *accumulator) add(series []float64) {
	for i, v := range series {
		if math.IsNaN(v) {
			continue
		}
		if a.counts[i] == 0 {
			a.vals[i] = v
		} else {
			switch a.agg {
			case Avg, Sum:
				a.vals[i] += v
			case Min:
				a.vals[i] = min(a.vals[i], v)
			case Max:
				a.vals[i] = max(a.vals[i], v)
			}
		}
		a.counts[i]++
	}
}

func (a *accumulator) value(i int) float64 {
	switch {
	case a.counts[i] == 0:
		return math.NaN()
	case a.agg == Avg:
		return a.vals[i] / float64(a.counts[i])
	default:
		return a.vals[i]
	}
}

// Label renders a series the way the UI names it: metric{k:v,…} with keys
// sorted, or metric{*} when there is no group-by (the spelling for
// "everything, aggregated").
func Label(s Series) string {
	if len(s.Tags) == 0 {
		return s.Metric + "{*}"
	}
	keys := make([]string, 0, len(s.Tags))
	for k := range s.Tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for i, k := range keys {
		keys[i] = wire.JoinTag(k, s.Tags[k])
	}
	return s.Metric + "{" + strings.Join(keys, ",") + "}"
}

// ParseFilter parses the API's filter syntax: comma-separated "key:value"
// terms, where a value containing '*' is a wildcard, a leading '!' negates
// the term, and a bare "key" matches the bare tag. Values cannot contain ','
// — the same rule the statsd tag format imposes.
func ParseFilter(s string) ([]tsdb.Matcher, error) {
	var out []tsdb.Matcher
	for term := range strings.SplitSeq(s, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		neg := strings.HasPrefix(term, "!")
		term = strings.TrimPrefix(term, "!")
		k, v := wire.SplitTag(term)
		if k == "" {
			return nil, fmt.Errorf("filter term %q has no tag key", term)
		}
		m := tsdb.Matcher{Key: k, Value: v, Type: tsdb.Equal}
		switch {
		case strings.Contains(v, "*") && neg:
			m.Type = tsdb.NotWildcard
		case strings.Contains(v, "*"):
			m.Type = tsdb.Wildcard
		case neg:
			m.Type = tsdb.NotEqual
		}
		out = append(out, m)
	}
	return out, nil
}

func floorTo(t, width int64) int64 {
	q := t / width
	if t%width != 0 && t < 0 {
		q--
	}
	return q * width
}

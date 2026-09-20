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
	switch r.Agg {
	case "":
		r.Agg = Avg
	case Avg, Sum, Min, Max:
	default:
		errs = append(errs, fmt.Errorf("agg %q: want avg, sum, min or max", r.Agg))
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
	if r.Interval == 0 {
		r.Interval = DefaultInterval(r.From, r.To)
	}
	if n := (r.To-floorTo(r.From, r.Interval))/r.Interval + 1; n > MaxBuckets {
		return fmt.Errorf("%d buckets at interval %ds; the limit is %d — use a larger interval or a shorter range", n, r.Interval, MaxBuckets)
	}
	return nil
}

// Run evaluates req against store.
func Run(ctx context.Context, store tsdb.MetricStore, req Request) (Result, error) {
	if err := req.Validate(); err != nil {
		return Result{}, err
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
	slices.SortFunc(res.Series, func(a, b Series) int { return strings.Compare(Label(a), Label(b)) })
	return res, nil
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

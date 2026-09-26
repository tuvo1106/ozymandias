package eval

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
)

// call evaluates a function.
//
// The parser has already checked the name and the shape of the arguments
// against the table behind [metricql.Lookup], so anything reaching here is
// spelled right and every switch arm below has a matching entry in it. A
// function in one and not the other is the bug that arrangement exists to make
// impossible, and the default arm is what says so out loud.
func (e *Evaluator) call(ctx context.Context, c *metricql.Call, g grid, st *state) (frame, error) {
	// timeshift is the one function that changes what its argument is
	// evaluated over rather than what comes back, so it runs before the
	// argument does.
	if c.Func == "timeshift" {
		return e.timeshift(ctx, c, g, st)
	}
	// A string argument names a mode — a reducer, a sort direction — and is
	// read straight from the AST by whichever function asked for it. It is not
	// a value and has nothing to evaluate, so it gets an empty slot rather
	// than a trip through node, which would rightly refuse it.
	args := make([]frame, len(c.Args))
	for i, a := range c.Args {
		if _, isString := a.(*metricql.String); isString {
			continue
		}
		f, err := e.node(ctx, a, g, st)
		if err != nil {
			return frame{}, err
		}
		args[i] = f
	}
	label := c.String()
	switch c.Func {
	case "abs":
		return pointwise(args[0], label, math.Abs), nil
	case "log2":
		return pointwise(args[0], label, positiveLog(math.Log2)), nil
	case "log10":
		return pointwise(args[0], label, positiveLog(math.Log10)), nil
	case "clamp_min":
		lo := literal(c.Args[1])
		return pointwise(args[0], label, func(v float64) float64 { return max(v, lo) }), nil
	case "clamp_max":
		hi := literal(c.Args[1])
		return pointwise(args[0], label, func(v float64) float64 { return min(v, hi) }), nil
	case "diff":
		return perLine(args[0], label, diff), nil
	case "moving_avg":
		n := int(literal(c.Args[1]))
		if n < 1 {
			return frame{}, fmt.Errorf("moving_avg needs a window of at least one bucket, not %v", literal(c.Args[1]))
		}
		return perLine(args[0], label, func(v []float64) []float64 { return movingAvg(v, n) }), nil
	case "top":
		return top(args[0], label, c)
	case "histogram_quantile":
		return histogramQuantile(args[1], label, literal(c.Args[0]), g)
	}
	return frame{}, fmt.Errorf("%s parses but is not implemented", c.Func)
}

// literal reads a number-shaped argument. The parser guarantees the shape, so
// this cannot fail; a zero here would mean the signature table and the switch
// above disagree about which argument is which.
func literal(n metricql.Node) float64 {
	switch v := n.(type) {
	case *metricql.Number:
		return v.Value
	case *metricql.Unary:
		if inner, ok := v.X.(*metricql.Number); ok {
			return -inner.Value
		}
	}
	return 0
}

// pointwise applies fn to every bucket of every line.
func pointwise(f frame, label string, fn func(float64) float64) frame {
	if f.isScalar {
		return frame{scalar: fn(f.scalar), isScalar: true, metric: label}
	}
	out := frame{metric: label, groups: make([]*group, 0, len(f.groups))}
	for _, grp := range f.groups {
		out.groups = append(out.groups, mapValues(grp, fn))
	}
	return out
}

// perLine applies fn to a whole line at once, for the functions whose answer
// at one bucket depends on its neighbours.
func perLine(f frame, label string, fn func([]float64) []float64) frame {
	if f.isScalar {
		// A scalar is a flat line; every one of these functions is either
		// zero or the identity on one, and fn says which.
		flat := fn([]float64{f.scalar, f.scalar})
		return frame{scalar: flat[len(flat)-1], isScalar: true, metric: label}
	}
	out := frame{metric: label, groups: make([]*group, 0, len(f.groups))}
	for _, grp := range f.groups {
		out.groups = append(out.groups, &group{tags: grp.tags, values: fn(grp.values)})
	}
	return out
}

// positiveLog guards a logarithm's domain. log of zero or a negative is not a
// number, and a null is the honest way to draw "there is no answer here"
// rather than -Inf, which would rescale the chart to nothing.
func positiveLog(fn func(float64) float64) func(float64) float64 {
	return func(v float64) float64 {
		if v <= 0 {
			return math.NaN()
		}
		return fn(v)
	}
}

// diff is the change from the previous bucket. The first bucket has no
// predecessor, so it is null rather than the value itself — a running total's
// first reading is not its increment.
//
// A null on either side makes the difference null: the gap might have held a
// change of any size.
func diff(v []float64) []float64 {
	out := make([]float64, len(v))
	for i := range v {
		if i == 0 || math.IsNaN(v[i]) || math.IsNaN(v[i-1]) {
			out[i] = math.NaN()
			continue
		}
		out[i] = v[i] - v[i-1]
	}
	return out
}

// movingAvg is the mean of the last n buckets, nulls skipped. A bucket with no
// real value in its window stays null: smoothing should not invent data where
// there was none, only smooth what is there.
func movingAvg(v []float64, n int) []float64 {
	out := make([]float64, len(v))
	for i := range v {
		var sum float64
		var count int
		for j := max(0, i-n+1); j <= i; j++ {
			if math.IsNaN(v[j]) {
				continue
			}
			sum += v[j]
			count++
		}
		if count == 0 {
			out[i] = math.NaN()
			continue
		}
		out[i] = sum / float64(count)
	}
	return out
}

// top keeps the n highest (or lowest) lines, ranked by reducing each to one
// number.
//
// It exists because a chart of four hundred containers is not a chart. Ranking
// happens after everything else, so `top(…, 5)` is the five busiest lines of
// the query as written rather than of the raw series.
func top(f frame, label string, c *metricql.Call) (frame, error) {
	if f.isScalar {
		return frame{scalar: f.scalar, isScalar: true, metric: label}, nil
	}
	n := int(literal(c.Args[1]))
	if n < 1 {
		return frame{}, fmt.Errorf("top needs a count of at least one, not %v", literal(c.Args[1]))
	}
	reducer, order := "mean", "desc"
	if len(c.Args) > 2 {
		reducer = c.Args[2].(*metricql.String).Value
	}
	if len(c.Args) > 3 {
		order = c.Args[3].(*metricql.String).Value
	}
	type ranked struct {
		g     *group
		score float64
	}
	scored := make([]ranked, 0, len(f.groups))
	for _, grp := range f.groups {
		scored = append(scored, ranked{g: grp, score: reduce(grp.values, reducer)})
	}
	// A line that is null throughout has no score and cannot be ranked
	// against one that has; it sorts last whichever direction is asked for,
	// so that `asc` does not fill up with empty lines.
	slices.SortStableFunc(scored, func(a, b ranked) int {
		switch {
		case math.IsNaN(a.score) && math.IsNaN(b.score):
			return strings.Compare(describe(a.g), describe(b.g))
		case math.IsNaN(a.score):
			return 1
		case math.IsNaN(b.score):
			return -1
		case a.score == b.score:
			return strings.Compare(describe(a.g), describe(b.g))
		case (a.score > b.score) == (order == "desc"):
			return -1
		default:
			return 1
		}
	})
	out := frame{metric: label}
	for i, r := range scored {
		if i == n {
			break
		}
		out.groups = append(out.groups, r.g)
	}
	return out, nil
}

// reduce collapses a line to the one number a ranking compares. Nulls are
// skipped; a line of nothing but nulls reduces to NaN, which top sorts last.
func reduce(v []float64, how string) float64 {
	var sum, best float64
	count := 0
	last := math.NaN()
	for _, x := range v {
		if math.IsNaN(x) {
			continue
		}
		if count == 0 {
			best = x
		}
		switch how {
		case "min":
			best = min(best, x)
		case "max":
			best = max(best, x)
		}
		sum += x
		last = x
		count++
	}
	if count == 0 {
		return math.NaN()
	}
	switch how {
	case "sum":
		return sum
	case "min", "max":
		return best
	case "last":
		return last
	default:
		return sum / float64(count)
	}
}

// timeshift evaluates its argument over a window moved by a number of seconds
// and reports it against the original one, so last week's line can be drawn
// under this week's.
//
// The shift is applied to the grid rather than to the result, because the data
// for a different window has to be *read* from that window. The returned
// points then carry the original timestamps: a chart overlays them, and that
// is the whole point of the function.
func (e *Evaluator) timeshift(ctx context.Context, c *metricql.Call, g grid, st *state) (frame, error) {
	offset := int64(literal(c.Args[1]))
	// Snap to the grid. An offset that is not a whole number of buckets would
	// compare bucket [0,60) against [-90,-30), which is not the same shape of
	// window and would show as a phase error nobody could explain.
	if offset%g.interval != 0 {
		return frame{}, fmt.Errorf(
			"timeshift by %ds is not a whole number of %ds buckets; use a multiple of the interval so the two windows line up",
			offset, g.interval)
	}
	shifted := grid{first: g.first + offset, interval: g.interval, n: g.n}
	if shifted.first < 0 {
		return frame{}, fmt.Errorf("timeshift by %ds moves the window before the epoch", offset)
	}
	f, err := e.node(ctx, c.Args[0], shifted, st)
	if err != nil {
		return frame{}, err
	}
	f.metric = c.String()
	return f, nil
}

// histogramQuantile interpolates a quantile from Prometheus-style cumulative
// buckets, whose upper bounds are a tag on the series.
//
// This is the path for a metric scraped from an exporter, where the buckets are
// all there is. The pXX aggregators are the path for a metric that arrives as a
// sketch. Both exist so that the same data can be measured two ways and the
// error between them reported rather than assumed — see docs/notes.
//
// The interpolation is linear within the bucket the quantile falls in, which is
// what Prometheus does and is wrong by up to the bucket's width. That is the
// method's error, not an implementation shortcut: a cumulative histogram does
// not record where inside a bucket its observations were.
func histogramQuantile(f frame, label string, q float64, g grid) (frame, error) {
	if q < 0 || q > 1 {
		return frame{}, fmt.Errorf("histogram_quantile needs a quantile in [0,1], not %v", q)
	}
	if f.isScalar {
		return frame{}, fmt.Errorf("histogram_quantile needs a query grouped by %q, not a number", upperBound)
	}
	// Group the lines by everything except the bucket boundary: each of those
	// is one histogram, spread over as many lines as it has buckets.
	type hist struct {
		tags   map[string]string
		bounds []float64
		lines  []*group
	}
	hists := map[string]*hist{}
	var order []string
	for _, grp := range f.groups {
		raw, ok := grp.tags[upperBound]
		if !ok {
			return frame{}, fmt.Errorf(
				"histogram_quantile needs the bucket boundary in the grouping: add `by {%s}` to the query it is given",
				upperBound)
		}
		bound, err := parseBound(raw)
		if err != nil {
			return frame{}, err
		}
		rest := make(map[string]string, len(grp.tags)-1)
		for k, v := range grp.tags {
			if k != upperBound {
				rest[k] = v
			}
		}
		key := describe(&group{tags: rest})
		h := hists[key]
		if h == nil {
			h = &hist{tags: rest}
			hists[key] = h
			order = append(order, key)
		}
		h.bounds = append(h.bounds, bound)
		h.lines = append(h.lines, grp)
	}

	out := frame{metric: label}
	for _, key := range order {
		h := hists[key]
		idx := make([]int, len(h.bounds))
		for i := range idx {
			idx[i] = i
		}
		slices.SortFunc(idx, func(a, b int) int {
			switch {
			case h.bounds[a] < h.bounds[b]:
				return -1
			case h.bounds[a] > h.bounds[b]:
				return 1
			default:
				return 0
			}
		})
		grp := &group{tags: h.tags, values: make([]float64, g.n)}
		counts := make([]float64, len(idx))
		bounds := make([]float64, len(idx))
		for i, j := range idx {
			bounds[i] = h.bounds[j]
		}
		for b := range g.n {
			for i, j := range idx {
				counts[i] = h.lines[j].values[b]
			}
			grp.values[b] = interpolate(bounds, counts, q)
		}
		out.groups = append(out.groups, grp)
	}
	return out, nil
}

// upperBound is the tag an OpenMetrics histogram's bucket boundary is carried
// in, matching what the openmetrics check emits.
const upperBound = "upper_bound"

// parseBound reads a bucket boundary. "+Inf" is the last bucket of every
// Prometheus histogram and has to be accepted; a negative bound is legal and
// appears on any histogram of a signed quantity.
func parseBound(s string) (float64, error) {
	switch strings.ToLower(s) {
	case "+inf", "inf":
		return math.Inf(1), nil
	case "-inf":
		return math.Inf(-1), nil
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%g", &v); err != nil {
		return 0, fmt.Errorf("%s=%q is not a bucket boundary", upperBound, s)
	}
	return v, nil
}

// interpolate finds the quantile in one bucket's worth of cumulative counts.
//
// bounds is ascending and counts is cumulative — counts[i] observations at or
// below bounds[i] — which is what a Prometheus `_bucket` series holds. The
// total is the last bucket, which is the +Inf one.
//
// The lowest bucket has no lower edge recorded anywhere, so **zero is assumed**
// to be its floor. That is what Prometheus assumes and it is right for the
// things histograms usually measure — durations, sizes, counts — but it is an
// assumption, not a measurement: a quantile landing in the first bucket of a
// histogram of temperatures would be interpolated from a floor nobody stated.
// The one case where the assumption is visibly wrong is a first bucket whose
// upper bound is itself at or below zero, and there the bound is returned as-is
// rather than interpolated backwards from an imaginary zero.
func interpolate(bounds, counts []float64, q float64) float64 {
	total := math.NaN()
	for i := len(counts) - 1; i >= 0; i-- {
		if !math.IsNaN(counts[i]) {
			total = counts[i]
			break
		}
	}
	if math.IsNaN(total) || total <= 0 {
		return math.NaN()
	}
	rank := q * total
	lowerBound, lowerCount := 0.0, 0.0
	first := true
	for i, c := range counts {
		if math.IsNaN(c) {
			continue
		}
		if c < rank {
			lowerBound, lowerCount = bounds[i], c
			first = false
			continue
		}
		switch {
		case math.IsInf(bounds[i], 1):
			// The quantile is in the open-ended bucket. There is no upper
			// bound to interpolate towards, so the best answer available is
			// the largest boundary that does have one.
			return lowerBound
		case first && bounds[i] <= 0:
			// See the note above: interpolating from an assumed zero floor
			// towards a bound at or below zero would run backwards.
			return bounds[i]
		case c == lowerCount:
			return bounds[i]
		}
		return lowerBound + (bounds[i]-lowerBound)*(rank-lowerCount)/(c-lowerCount)
	}
	return lowerBound
}

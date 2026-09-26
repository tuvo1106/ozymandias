package eval

import (
	"context"
	"fmt"
	"math"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/internal/tsdb"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// query evaluates one `agg:metric{filter} by {keys}.modifiers` node.
func (e *Evaluator) query(ctx context.Context, q *metricql.Query, g grid, st *state) (frame, error) {
	kind := e.metricKind(q.Metric)
	if _, isPercentile := q.Agg.Quantile(); isPercentile {
		return e.percentile(ctx, q, g, st, kind)
	}
	if kind == wire.KindDistribution {
		return frame{}, fmt.Errorf(
			"%s is a distribution, so %s is not a number it has: use p50, p75, p90, p95 or p99, or query %s%s, %s%s, %s%s or %s%s",
			q.Metric, q.Agg, q.Metric, wire.SuffixCount, q.Metric, wire.SuffixSum,
			q.Metric, wire.SuffixMin, q.Metric, wire.SuffixMax)
	}

	sel, post, err := e.selector(q, st)
	if err != nil {
		return frame{}, err
	}
	method := rollupFor(q, defaultRollup(kind))

	set, err := e.Store.Select(ctx, sel, g.first*1000, g.endMs()-1)
	if err != nil {
		return frame{}, err
	}
	defer set.Close()

	groups := map[string]*accumulator{}
	var order []string
	selected := 0
	for set.Next() {
		ref := set.Series()
		if !post.matches(ref) {
			continue
		}
		if selected++; selected > MaxSeriesPerNode {
			return frame{}, tooManySeries(q)
		}
		values := bucketize(set.Iterator(), g, method)
		if values == nil {
			continue
		}
		key, tags := groupOf(ref, q.By)
		acc := groups[key]
		if acc == nil {
			acc = newAccumulator(q.Agg, g.n, tags)
			groups[key] = acc
			order = append(order, key)
		}
		acc.add(values)
	}
	if err := set.Err(); err != nil {
		return frame{}, err
	}

	f := frame{metric: q.Metric, groups: make([]*group, 0, len(order))}
	for _, key := range order {
		f.groups = append(f.groups, groups[key].group())
	}
	return applyModifiers(f, q, g, kind)
}

func tooManySeries(q *metricql.Query) error {
	return fmt.Errorf(
		"%s selects more than %d series; narrow the filter, or group with `by {…}` so that the answer is a few lines rather than thousands",
		q, MaxSeriesPerNode)
}

// selector turns a filter into what the store can index on, plus what has to
// be checked afterwards.
//
// An `IN` list is a disjunction over one key's values, and [tsdb.Matcher] has
// no disjunction: it compares one key against one value or glob. Rather than
// widen the store's matcher — which every store, the index and the
// differential tests would have to agree on again — an IN list narrows to
// "has this key at all" and the membership test runs here. It reads more
// series than it keeps; a filter that can be pushed down still is.
func (e *Evaluator) selector(q *metricql.Query, st *state) (tsdb.Selector, postFilter, error) {
	sel := tsdb.Selector{Metric: q.Metric}
	var post postFilter
	for _, m := range q.Filter {
		if m.Var != "" {
			expanded, err := expandVar(m, st)
			if err != nil {
				return sel, post, err
			}
			sel.Matchers = append(sel.Matchers, expanded...)
			continue
		}
		if m.In {
			post = append(post, m)
			if !m.Neg {
				// Requiring the key narrows the read without deciding the
				// membership question, which post does.
				sel.Matchers = append(sel.Matchers, tsdb.Matcher{Key: m.Key, Value: "*", Type: tsdb.Wildcard})
			}
			continue
		}
		sel.Matchers = append(sel.Matchers, matcherFor(m.Key, m.Values[0], m.Neg))
	}
	return sel, post, nil
}

// matcherFor maps one `key:value` term. A value containing '*' is a glob; the
// store's GlobMatch is the one implementation of that.
func matcherFor(key, value string, neg bool) tsdb.Matcher {
	t := tsdb.Equal
	switch {
	case containsStar(value) && neg:
		t = tsdb.NotWildcard
	case containsStar(value):
		t = tsdb.Wildcard
	case neg:
		t = tsdb.NotEqual
	}
	return tsdb.Matcher{Key: key, Value: value, Type: t}
}

func containsStar(s string) bool {
	for i := range len(s) {
		if s[i] == '*' {
			return true
		}
	}
	return false
}

// expandVar resolves a `$name` to the matchers a dashboard bound it to.
//
// An unbound variable is an error: a dashboard that forgot to send `env` would
// otherwise silently widen its query to every environment, which is the kind
// of wrong answer that gets believed. A variable bound to nothing is *not* an
// error — that is what a template variable's "all" selection is — and it adds
// no matcher.
func expandVar(m metricql.Matcher, st *state) ([]tsdb.Matcher, error) {
	values, ok := st.vars[m.Var]
	if !ok {
		return nil, fmt.Errorf("$%s is not bound: the query uses it as a filter but the request supplied no value", m.Var)
	}
	if len(values) == 0 {
		st.warnf("$%s resolved to no filter, so every value of it is included", m.Var)
		return nil, nil
	}
	out := make([]tsdb.Matcher, 0, len(values))
	for _, v := range values {
		tag := tsdb.ParseTag(v)
		if tag.Key == "" {
			return nil, fmt.Errorf("$%s is bound to %q, which is not a key:value tag", m.Var, v)
		}
		out = append(out, matcherFor(tag.Key, tag.Value, false))
	}
	return out, nil
}

// postFilter holds the matchers that could not be pushed into the store.
type postFilter []metricql.Matcher

func (p postFilter) matches(ref tsdb.SeriesRef) bool {
	for _, m := range p {
		value, has := ref.Get(m.Key)
		in := has && containsValue(m.Values, value)
		// A negated list excludes the values it names; a series without the
		// key is not one of them, so it stays — the same reading NotEqual has.
		if in == m.Neg {
			return false
		}
	}
	return true
}

func containsValue(values []string, v string) bool {
	for _, want := range values {
		if want == v || (containsStar(want) && tsdb.GlobMatch(want, v)) {
			return true
		}
	}
	return false
}

// rollupFor picks the time aggregation: what `.rollup()` names, else the
// metric type's default.
func rollupFor(q *metricql.Query, def rollup) rollup {
	for _, m := range q.Modifiers {
		if m.Kind == metricql.ModRollup && m.Method != "" {
			return rollup(m.Method)
		}
	}
	return def
}

// bucketize reduces one series' samples to one value per bucket. It returns
// nil when no sample landed on the grid at all, so that a series the store
// returned but the window does not cover draws no line.
func bucketize(it tsdb.SeriesIterator, g grid, method rollup) []float64 {
	values := make([]float64, g.n)
	counts := make([]int, g.n)
	any := false
	for it.Next() {
		s := it.At()
		i, ok := g.index(s.T / 1000)
		if !ok {
			continue
		}
		switch {
		case counts[i] == 0:
			values[i] = s.V
		case method == rollupSum || method == rollupAvg:
			values[i] += s.V
		case method == rollupMin:
			values[i] = min(values[i], s.V)
		case method == rollupMax:
			values[i] = max(values[i], s.V)
		case method == rollupLast:
			// The store returns a series in timestamp order, so the last one
			// to arrive is the latest.
			values[i] = s.V
		}
		counts[i]++
		any = true
	}
	if !any {
		return nil
	}
	for i := range values {
		switch {
		case counts[i] == 0:
			values[i] = math.NaN()
		case method == rollupAvg:
			values[i] /= float64(counts[i])
		case method == rollupCount:
			values[i] = float64(counts[i])
		}
	}
	return values
}

// groupOf computes a group's key and tags from a series and the by keys.
func groupOf(ref tsdb.SeriesRef, by []string) (string, map[string]string) {
	tags := make(map[string]string, len(by))
	key := make([]byte, 0, 16*len(by))
	for _, k := range by {
		key = append(key, k...)
		if v, ok := ref.Get(k); ok {
			tags[k] = v
			key = append(key, '=')
			key = append(key, v...)
		}
		key = append(key, 0)
	}
	return string(key), tags
}

// accumulator folds a group's series into one line, bucket by bucket, skipping
// nulls. A bucket where every series was null stays null.
type accumulator struct {
	agg    metricql.Agg
	tags   map[string]string
	values []float64
	counts []int
}

func newAccumulator(agg metricql.Agg, n int, tags map[string]string) *accumulator {
	return &accumulator{agg: agg, tags: tags, values: make([]float64, n), counts: make([]int, n)}
}

func (a *accumulator) add(series []float64) {
	for i, v := range series {
		if math.IsNaN(v) {
			continue
		}
		switch {
		case a.counts[i] == 0:
			a.values[i] = v
		case a.agg == metricql.Avg || a.agg == metricql.Sum:
			a.values[i] += v
		case a.agg == metricql.Min:
			a.values[i] = min(a.values[i], v)
		case a.agg == metricql.Max:
			a.values[i] = max(a.values[i], v)
		}
		a.counts[i]++
	}
}

func (a *accumulator) group() *group {
	out := &group{tags: a.tags, values: make([]float64, len(a.values))}
	for i := range a.values {
		switch {
		case a.counts[i] == 0:
			out.values[i] = math.NaN()
		case a.agg == metricql.Count:
			// How many series reported in this bucket — the cardinality of
			// the group, which is what "count" across series means.
			out.values[i] = float64(a.counts[i])
		case a.agg == metricql.Avg:
			out.values[i] = a.values[i] / float64(a.counts[i])
		default:
			out.values[i] = a.values[i]
		}
	}
	return out
}

package eval

import (
	"fmt"
	"math"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// applyModifiers runs a query's modifiers in the order written.
//
// rollup was consumed earlier — it decides the time aggregation and the grid,
// both of which had to be known before any sample was read — so what is left
// here is the three that transform a finished line.
func applyModifiers(f frame, q *metricql.Query, g grid, kind wire.Kind, st *state) (frame, error) {
	_, isPercentile := q.Agg.Quantile()
	for _, m := range q.Modifiers {
		switch m.Kind {
		case metricql.ModRollup:
			// The width was consumed by the planner and the method by the
			// time aggregation — except on a percentile, which has no choice
			// of method: its buckets are merged sketches, and a merge is the
			// only reduction that keeps the error bound. Saying so is the
			// point. The milestone's own example writes `.rollup(max, 60)` on
			// a p95, and silently dropping half of what somebody typed is
			// exactly the confident-wrong-answer habit this package keeps
			// deciding against.
			if isPercentile && m.Method != "" {
				st.warnf(
					"%s merges sketches, so the rollup method %q is not used; the bucket width still is",
					q.Agg, m.Method)
			}
		case metricql.ModAsRate, metricql.ModAsCount:
			if err := rateApplies(q, m.Kind, kind); err != nil {
				return frame{}, err
			}
			scale := 1 / float64(g.interval)
			if m.Kind == metricql.ModAsCount {
				// A count is already the count over its bucket, so as_count
				// is the identity on one; on a rate it is the rate times the
				// width. Making it the identity rather than an error lets a
				// dashboard write `.as_count()` on a metric it does not know
				// the type of and get the number it meant either way.
				if kind != wire.KindRate {
					continue
				}
				scale = float64(g.interval)
			}
			for _, grp := range f.groups {
				for i, v := range grp.values {
					grp.values[i] = v * scale
				}
			}
		case metricql.ModFill:
			fill(f, fillMode(m.Method), m.Seconds, g)
		default:
			return frame{}, fmt.Errorf("unknown modifier %q", m.Kind)
		}
	}
	return f, nil
}

// rateApplies refuses as_rate and as_count on a metric where the answer would
// be a number with no meaning. Dividing a temperature by sixty is arithmetic,
// not a rate.
func rateApplies(q *metricql.Query, mod metricql.ModKind, kind wire.Kind) error {
	switch kind {
	case wire.KindCount, wire.KindRate:
		return nil
	case "":
		// Unknown: the metric has not been seen, so there is nothing to
		// contradict. The query returns no data anyway.
		return nil
	default:
		return fmt.Errorf("%s is a %s, so .%s() does not apply to it — only a count or a rate has one",
			q.Metric, kind, mod)
	}
}

// fillMode is how empty buckets are replaced.
type fillMode string

// The fill modes, matching the grammar's.
const (
	fillNull   fillMode = "null"
	fillZero   fillMode = "zero"
	fillLast   fillMode = "last"
	fillLinear fillMode = "linear"
)

// fill replaces null buckets in place.
//
// limit, in seconds, bounds how far a filled bucket may be from the real data
// behind it: a gauge that stopped reporting an hour ago should not still be
// drawn at its last value. It is a distance per bucket, not a maximum gap
// length — a twenty-minute hole with `fill(last, 300)` is filled for five
// minutes and then stops, which is what "fill for up to five minutes" means.
// Zero is no bound, which is what `.fill(last)` alone asks for. It applies to
// **every** mode including `zero`: drawing a flat zero line forever after a
// series stopped reporting is the most misleading of the four, because it
// looks like a measurement of nothing happening rather than an absence of
// measurement.
//
// A run of nulls at the *start* of the window has nothing behind it, so `last`
// and `linear` leave it alone: inventing a value there would be inventing
// history. `linear` likewise leaves a trailing run alone — there is no second
// point to interpolate towards, and continuing the last slope off the end of
// the data is extrapolation dressed as measurement.
func fill(f frame, mode fillMode, limit int64, g grid) {
	if mode == fillNull || mode == "" {
		return
	}
	reach := g.n
	if limit > 0 {
		reach = int(limit / g.interval)
	}
	for _, grp := range f.groups {
		v := grp.values
		for i := 0; i < len(v); i++ {
			if !math.IsNaN(v[i]) {
				continue
			}
			// The gap is [i, j), with v[i-1] the real value behind it.
			j := i
			for j < len(v) && math.IsNaN(v[j]) {
				j++
			}
			switch {
			case i == 0:
				// Nothing behind this run. `zero` is the one mode that does
				// not need a value behind it — it asserts that absence means
				// zero rather than carrying anything forward — so it fills,
				// but only when no limit was given: a limit is a statement
				// about distance from real data, and here there is none to
				// measure from.
				if mode == fillZero && limit == 0 {
					for k := range j {
						v[k] = 0
					}
				}
			case mode == fillZero:
				for k := i; k < j && k-i < reach; k++ {
					v[k] = 0
				}
			case mode == fillLast:
				for k := i; k < j && k-i < reach; k++ {
					v[k] = v[i-1]
				}
			case mode == fillLinear && j < len(v):
				// linear needs a point ahead to aim at; without one the gap
				// stays a gap.
				lo, step := v[i-1], (v[j]-v[i-1])/float64(j-i+1)
				for k := i; k < j && k-i < reach; k++ {
					v[k] = lo + step*float64(k-i+1)
				}
			}
			i = j - 1
		}
	}
}

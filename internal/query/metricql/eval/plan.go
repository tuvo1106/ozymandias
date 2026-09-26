package eval

import (
	"fmt"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// maxTime is the newest accepted timestamp, 9999-12-31T23:59:59Z. Unix seconds
// are unbounded but the bucket arithmetic is not — it multiplies back out to
// milliseconds — so the window is checked before anything computes with it.
const maxTime = 253402300799

// DefaultInterval aims for about 300 points across the range, in whole
// multiples of the agent's ten-second flush: a bucket narrower than the data's
// own resolution is just a bucket that is mostly empty.
func DefaultInterval(from, to int64) int64 {
	iv := (to - from + 299) / 300
	iv = (iv + 9) / 10 * 10
	return max(iv, 10)
}

// plan validates the request and chooses the grid every node will share.
//
// The interval comes from the first of these that the request has: the
// explicit Interval, a `.rollup(_, seconds)` written on a query, or
// DefaultInterval. Two rollups asking for different widths is an error rather
// than a resampling, and an explicit Interval that a rollup contradicts is an
// error too — see docs/adr/0016-one-grid-per-query.md. The alternative is
// inventing a rule for what `a.rollup(avg,60) + b.rollup(avg,300)` means, and
// every such rule surprises somebody.
func (e *Evaluator) plan(req Request) (grid, error) {
	if req.Expr == nil {
		return grid{}, fmt.Errorf("no query to evaluate")
	}
	if req.To <= req.From {
		return grid{}, fmt.Errorf("to (%d) must be after from (%d)", req.To, req.From)
	}
	if req.From < 0 || req.To > maxTime {
		return grid{}, fmt.Errorf("from (%d) and to (%d) must be unix seconds within [0, %d]", req.From, req.To, maxTime)
	}
	if req.Interval < 0 {
		return grid{}, fmt.Errorf("interval %d must be positive", req.Interval)
	}

	interval := req.Interval
	source := "the interval parameter"
	var err error
	walk(req.Expr, func(n metricql.Node) {
		q, ok := n.(*metricql.Query)
		if !ok || err != nil {
			return
		}
		for _, m := range q.Modifiers {
			if m.Kind != metricql.ModRollup || m.Seconds == 0 {
				continue
			}
			switch {
			case interval == 0:
				interval, source = m.Seconds, fmt.Sprintf("the rollup on %s", q)
			case interval != m.Seconds:
				err = fmt.Errorf(
					"%s asks for %ds buckets but %s asks for %ds: one query answers on one set of buckets, so these cannot be combined",
					source, interval, fmt.Sprintf("the rollup on %s", q), m.Seconds)
			}
		}
	})
	if err != nil {
		return grid{}, err
	}
	if interval == 0 {
		interval = DefaultInterval(req.From, req.To)
	}

	first := floorTo(req.From, interval)
	n := (req.To-first)/interval + 1
	if n > MaxBuckets {
		return grid{}, fmt.Errorf(
			"%d buckets at %ds; the limit is %d — use a coarser interval or a shorter range",
			n, interval, MaxBuckets)
	}
	return grid{first: first, interval: interval, n: int(n)}, nil
}

// walk calls fn on n and every node beneath it, parents first.
func walk(n metricql.Node, fn func(metricql.Node)) {
	fn(n)
	switch v := n.(type) {
	case *metricql.Unary:
		walk(v.X, fn)
	case *metricql.Binary:
		walk(v.Left, fn)
		walk(v.Right, fn)
	case *metricql.Call:
		for _, a := range v.Args {
			walk(a, fn)
		}
	}
}

// rollup is how a bucket's samples reduce to one value.
type rollup string

// The rollup methods, matching the grammar's.
const (
	rollupAvg   rollup = "avg"
	rollupSum   rollup = "sum"
	rollupMin   rollup = "min"
	rollupMax   rollup = "max"
	rollupCount rollup = "count"
	rollupLast  rollup = "last"
)

// defaultRollup is how a metric's samples reduce over time when the query does
// not say.
//
// A count sums: ten ten-second counts are one hundred-second count, and that
// is the only reading under which "requests in the last hour" is a number. A
// gauge averages: it is a level, and the level over a bucket is its mean.
// A rate sums, which is the plan's choice and is what M1's engine does — it
// makes `.as_rate()` on either kind mean the same thing.
func defaultRollup(kind wire.Kind) rollup {
	switch kind {
	case wire.KindCount, wire.KindRate:
		return rollupSum
	default:
		return rollupAvg
	}
}

// metricKind looks up a metric's recorded type. An unknown metric is not an
// error: it may simply not have been written yet, and a query for it should
// return nothing rather than refuse. The empty kind reads as a gauge for time
// aggregation, and is not a distribution, so percentiles on it are refused
// with a reason.
func (e *Evaluator) metricKind(name string) wire.Kind {
	if e.Types == nil {
		return ""
	}
	if md, ok := e.Types.Metric(name); ok {
		return md.Type
	}
	return ""
}

func floorTo(t, width int64) int64 {
	q := t / width
	if t%width != 0 && t < 0 {
		q--
	}
	return q * width
}

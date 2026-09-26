package eval

import (
	"context"
	"math"

	"github.com/tuvo1106/ozymandias/internal/query/metricql"
)

// binary evaluates `left op right`.
func (e *Evaluator) binary(ctx context.Context, b *metricql.Binary, g grid, st *state) (frame, error) {
	left, err := e.node(ctx, b.Left, g, st)
	if err != nil {
		return frame{}, err
	}
	right, err := e.node(ctx, b.Right, g, st)
	if err != nil {
		return frame{}, err
	}
	return join(left, right, b, g, st), nil
}

// join combines two frames pointwise.
//
// Two scalars make a scalar. A scalar and a set of lines broadcast: the number
// applies to every bucket of every line, which is how `… / 1000` and `… * 100`
// are written. Two sets of lines are matched **by group tag set**, and a group
// on one side with no partner on the other is dropped with a warning.
//
// Dropping rather than treating the missing side as zero is deliberate. In
// `errors / requests`, a route that served no requests at all has no rate to
// divide by; drawing it as an infinite or a zero error ratio would be
// inventing a fact about a route nothing is known about. The warning is there
// because silence about a dropped line is how a dashboard comes to show four
// of its six services and nobody notices.
func join(left, right frame, b *metricql.Binary, g grid, st *state) frame {
	op := operator(b.Op)
	label := b.String()
	switch {
	case left.isScalar && right.isScalar:
		return frame{scalar: op(left.scalar, right.scalar), isScalar: true, metric: label}
	case right.isScalar:
		out := frame{metric: label, groups: make([]*group, 0, len(left.groups))}
		for _, grp := range left.groups {
			out.groups = append(out.groups, mapValues(grp, func(v float64) float64 { return op(v, right.scalar) }))
		}
		return out
	case left.isScalar:
		out := frame{metric: label, groups: make([]*group, 0, len(right.groups))}
		for _, grp := range right.groups {
			out.groups = append(out.groups, mapValues(grp, func(v float64) float64 { return op(left.scalar, v) }))
		}
		return out
	}

	byKey := make(map[string]*group, len(right.groups))
	for _, grp := range right.groups {
		byKey[grp.key()] = grp
	}
	out := frame{metric: label}
	matched := map[string]bool{}
	for _, l := range left.groups {
		r, ok := byKey[l.key()]
		if !ok {
			st.warnf("%s: no match on the right for %s, so that line is not drawn", label, describe(l))
			continue
		}
		matched[l.key()] = true
		merged := &group{tags: l.tags, values: make([]float64, g.n)}
		for i := range merged.values {
			merged.values[i] = op(l.values[i], r.values[i])
		}
		out.groups = append(out.groups, merged)
	}
	for _, r := range right.groups {
		if !matched[r.key()] {
			st.warnf("%s: no match on the left for %s, so that line is not drawn", label, describe(r))
		}
	}
	return out
}

func describe(g *group) string {
	s := Series{Tags: g.tags}
	return s.Scope()
}

// operator returns the arithmetic for an operator.
//
// Every one of them propagates null: a bucket where either side is empty is
// empty in the result, because an operation on a value nobody measured has no
// answer. Division by zero is null for the same reason — a ratio against
// nothing measured is not infinity, it is unknown.
func operator(op metricql.Op) func(a, b float64) float64 {
	switch op {
	case metricql.OpAdd:
		return func(a, b float64) float64 { return a + b }
	case metricql.OpSub:
		return func(a, b float64) float64 { return a - b }
	case metricql.OpMul:
		return func(a, b float64) float64 { return a * b }
	default:
		return func(a, b float64) float64 {
			if b == 0 {
				return math.NaN()
			}
			return a / b
		}
	}
}

// negate applies unary minus.
func negate(f frame, n metricql.Node) frame {
	if f.isScalar {
		return frame{scalar: -f.scalar, isScalar: true, metric: n.String()}
	}
	out := frame{metric: n.String(), groups: make([]*group, 0, len(f.groups))}
	for _, grp := range f.groups {
		out.groups = append(out.groups, mapValues(grp, func(v float64) float64 { return -v }))
	}
	return out
}

// mapValues returns a group with fn applied to every bucket. NaN in, NaN out,
// without fn having to know: every caller here is arithmetic, and arithmetic
// on an unmeasured bucket is unmeasured.
func mapValues(g *group, fn func(float64) float64) *group {
	out := &group{tags: g.tags, values: make([]float64, len(g.values))}
	for i, v := range g.values {
		if math.IsNaN(v) {
			out.values[i] = math.NaN()
			continue
		}
		out.values[i] = fn(v)
	}
	return out
}

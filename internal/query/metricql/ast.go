package metricql

import (
	"math"
	"strconv"
	"strings"
)

// Node is one expression in a parsed query. The implementations are [*Number],
// [*String], [*Query], [*Call], [*Unary] and [*Binary].
//
// Nodes hold no source positions: see the package comment for why, and for
// what [Node.String] guarantees.
type Node interface {
	// String returns the node's canonical text, parenthesised exactly where
	// precedence requires. Parse(n.String()) equals n.
	String() string
	node()
}

// Number is a numeric literal.
type Number struct {
	Value float64
}

// String is a quoted string literal. It is only meaningful as a function
// argument — there is no operation on strings — so the parser accepts one
// only where a function's signature calls for it.
type String struct {
	Value string
}

// Op is a binary or unary operator, spelled as it is written.
type Op string

// The operators, in the two precedence levels the grammar has.
const (
	OpAdd Op = "+"
	OpSub Op = "-"
	OpMul Op = "*"
	OpDiv Op = "/"
)

// Binary is `left op right`.
type Binary struct {
	Op          Op
	Left, Right Node
}

// Unary is negation: `-x`. It is the one unary operator, and it exists so
// that a negative argument can be written at all (`timeshift(q, -3600)`),
// since a leading '-' cannot be lexed into the number — `a-1` would then be
// two terms with no operator between them.
type Unary struct {
	Op Op // always OpSub
	X  Node
}

// Call is a function application, `name(arg, …)`. The parser checks the name
// and the shape of the arguments against [Functions]; what a function
// computes is the evaluator's business.
type Call struct {
	Func string
	Args []Node
}

// Agg is the across-series aggregator a query opens with — the "space"
// aggregation, applied per bucket across the series of a group.
type Agg string

// The aggregators. The percentile ones are answerable only on a distribution
// metric, whose sketches merge; the rest apply to any series.
const (
	Avg   Agg = "avg"
	Sum   Agg = "sum"
	Min   Agg = "min"
	Max   Agg = "max"
	Count Agg = "count"
	P50   Agg = "p50"
	P75   Agg = "p75"
	P90   Agg = "p90"
	P95   Agg = "p95"
	P99   Agg = "p99"
)

// aggs is every aggregator the grammar accepts, and for a percentile, the
// quantile it means.
var aggs = map[Agg]float64{
	Avg: math.NaN(), Sum: math.NaN(), Min: math.NaN(),
	Max: math.NaN(), Count: math.NaN(),
	P50: 0.50, P75: 0.75, P90: 0.90, P95: 0.95, P99: 0.99,
}

// Quantile reports the quantile a percentile aggregator asks for, and whether
// a is one. A caller that needs to know "does this query need the sketch
// store?" is asking exactly this.
func (a Agg) Quantile() (float64, bool) {
	q, ok := aggs[a]
	if !ok || math.IsNaN(q) {
		return 0, false
	}
	return q, true
}

// Query is one selection: an aggregator, a metric, a filter, an optional
// grouping and a chain of modifiers. It is the leaf of an expression — every
// other node combines queries or numbers.
type Query struct {
	Agg       Agg
	Metric    string
	Filter    Filter
	By        []string
	Modifiers []Modifier
}

// Filter is the contents of `{…}`: the matchers a series must satisfy, ANDed.
// An empty Filter is the `{*}` filter, which selects every series of the
// metric — "no constraints" and "match everything" are the same thing, so
// there is no separate flag for it and nothing to keep consistent.
type Filter []Matcher

// Matcher is one term of a filter, in one of three shapes:
//
//   - a tag match, `key:value`, where the value may contain '*' wildcards;
//   - a set match, `key IN (a,b)`, which is an OR over values;
//   - a template variable, `$env`, which a dashboard resolves to zero or more
//     of the other two before the query is planned.
//
// Var is non-empty for the third shape, and then every other field is unset.
// Neg negates the first two ('!'); a variable cannot be negated, because what
// it expands to decides that.
type Matcher struct {
	Var    string
	Neg    bool
	Key    string
	In     bool
	Values []string
}

// ModKind is which modifier a [Modifier] is.
type ModKind string

// The modifiers. They apply in the order written, after time aggregation.
const (
	// ModRollup overrides the time aggregation: method, and optionally the
	// bucket width in seconds.
	ModRollup ModKind = "rollup"
	// ModAsRate divides each bucket by its width: a count becomes a per-second
	// rate.
	ModAsRate ModKind = "as_rate"
	// ModAsCount is the inverse: a rate becomes the count over the bucket.
	ModAsCount ModKind = "as_count"
	// ModFill replaces empty buckets, optionally only within a limit in
	// seconds.
	ModFill ModKind = "fill"
)

// Rollup methods, the ways a bucket's samples reduce to one value.
var rollupMethods = []string{"avg", "sum", "min", "max", "count", "last"}

// Fill modes. "null" is the default — a gap is drawn as a gap — and is worth
// writing explicitly only to override a dashboard-level default.
var fillModes = []string{"null", "zero", "last", "linear"}

// Modifier is one `.name(…)` in a query's chain.
type Modifier struct {
	Kind ModKind
	// Method is the rollup method or the fill mode; empty for as_rate and
	// as_count.
	Method string
	// Seconds is the optional second argument of rollup (bucket width) and
	// fill (how far a fill may reach). Zero means it was not written: the
	// parser rejects an explicit zero, so the two cannot be confused.
	Seconds int64
}

func (*Number) node() {}
func (*String) node() {}
func (*Query) node()  {}
func (*Call) node()   {}
func (*Unary) node()  {}
func (*Binary) node() {}

// String formats the literal the shortest way that reads back as the same
// float64 ('g' with precision -1), so printing and re-parsing is exact.
func (n *Number) String() string { return strconv.FormatFloat(n.Value, 'g', -1, 64) }

func (s *String) String() string { return strconv.Quote(s.Value) }

func (c *Call) String() string {
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = a.String()
	}
	return c.Func + "(" + strings.Join(args, ", ") + ")"
}

func (u *Unary) String() string {
	if precedence(u.X) < precUnary {
		return string(u.Op) + "(" + u.X.String() + ")"
	}
	return string(u.Op) + u.X.String()
}

// String parenthesises a child exactly where dropping the parentheses would
// re-parse into a different tree.
//
// On the left that means a child that binds less tightly. On the right it
// also means a child that binds *equally* tightly, for every operator and not
// only for the two that are not associative: the parser builds left-leaning
// trees, so `a + (b + c)` printed without its parentheses comes back as
// `(a + b) + c` — a different tree, and over float64 often a different
// number. The only way to hold such a tree in the first place is to have
// written those parentheses or built the node in code, and both mean them.
func (b *Binary) String() string {
	left, right := b.Left.String(), b.Right.String()
	if precedence(b.Left) < precedence(b) {
		left = "(" + left + ")"
	}
	if precedence(b.Right) <= precedence(b) {
		right = "(" + right + ")"
	}
	return left + " " + string(b.Op) + " " + right
}

func (q *Query) String() string {
	var b strings.Builder
	b.WriteString(string(q.Agg))
	b.WriteByte(':')
	b.WriteString(q.Metric)
	b.WriteByte('{')
	if len(q.Filter) == 0 {
		b.WriteByte('*')
	}
	for i, m := range q.Filter {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(m.String())
	}
	b.WriteByte('}')
	if len(q.By) > 0 {
		b.WriteString(" by {")
		b.WriteString(strings.Join(q.By, ","))
		b.WriteByte('}')
	}
	for _, m := range q.Modifiers {
		b.WriteByte('.')
		b.WriteString(m.String())
	}
	return b.String()
}

// String prints the matcher in the shape it was written in.
func (m Matcher) String() string {
	if m.Var != "" {
		return "$" + m.Var
	}
	var b strings.Builder
	if m.Neg {
		b.WriteByte('!')
	}
	b.WriteString(m.Key)
	if m.In {
		b.WriteString(" IN (")
		b.WriteString(strings.Join(m.Values, ","))
		b.WriteByte(')')
		return b.String()
	}
	b.WriteByte(':')
	if len(m.Values) > 0 {
		b.WriteString(m.Values[0])
	}
	return b.String()
}

// String prints the modifier, leaving out a second argument that was not
// written.
func (m Modifier) String() string {
	var b strings.Builder
	b.WriteString(string(m.Kind))
	b.WriteByte('(')
	b.WriteString(m.Method)
	if m.Seconds > 0 {
		if m.Method != "" {
			b.WriteString(", ")
		}
		b.WriteString(strconv.FormatInt(m.Seconds, 10))
	}
	b.WriteByte(')')
	return b.String()
}

// Precedence levels, lowest binding first. Everything that is not a binary
// operator binds tightest: a number, a query, a call and a negation are all
// single factors as far as the printer is concerned.
const (
	precAddSub = iota + 1
	precMulDiv
	precUnary
)

func precedence(n Node) int {
	b, ok := n.(*Binary)
	if !ok {
		return precUnary
	}
	if b.Op == OpAdd || b.Op == OpSub {
		return precAddSub
	}
	return precMulDiv
}

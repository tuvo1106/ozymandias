package metricql

import (
	"strconv"
	"strings"
)

// ArgKind is what one argument position of a function accepts.
type ArgKind int

// The argument kinds.
const (
	// ArgExpr is any expression: a query, a number, arithmetic, another call.
	ArgExpr ArgKind = iota
	// ArgNumber is a literal number, optionally negated. Deliberately not "an
	// expression that happens to be constant": `clamp_min(q, 2*3)` would have
	// to be folded somewhere, and one rule with no exceptions is easier to
	// explain in an error message than a constant-folding pass is to write.
	ArgNumber
	// ArgReducer is a quoted string naming how to reduce a series to the one
	// number a ranking compares.
	ArgReducer
	// ArgOrder is a quoted string naming a sort direction.
	ArgOrder
)

// Enum values for the string-shaped argument kinds.
var (
	reducers = []string{"mean", "sum", "min", "max", "last"}
	orders   = []string{"asc", "desc"}
)

func (k ArgKind) values() []string {
	switch k {
	case ArgReducer:
		return reducers
	case ArgOrder:
		return orders
	default:
		return nil
	}
}

// Signature is a function's shape: what each argument position accepts, and
// how many of them are required. Arguments past Required are optional and
// must still come in order.
type Signature struct {
	Args     []ArgKind
	Required int
}

// functions is every function the language has, and the shape of its
// arguments. It is the parser's authority for "is this spelled right?", and
// the evaluator's for "what am I being asked to compute" — one table, so a
// function cannot be accepted by one and unimplemented by the other.
//
// Unexported, and read through [Lookup]. An exported map would be writable by
// anything that imports this package while [Parse] is reading it on another
// goroutine, which is a data race no caller could fix from outside; it would
// also be the global mutable state AGENTS.md §4 rules out. Nothing needs to
// add a function at runtime — a function is code — so a read-only door is the
// whole of the API.
var functions = map[string]Signature{
	// Shape, per bucket and per series.
	"abs":   {Args: []ArgKind{ArgExpr}, Required: 1},
	"log2":  {Args: []ArgKind{ArgExpr}, Required: 1},
	"log10": {Args: []ArgKind{ArgExpr}, Required: 1},

	// Bounds. A clamp is how a dashboard keeps one bad point from flattening
	// a chart's whole y-axis.
	"clamp_min": {Args: []ArgKind{ArgExpr, ArgNumber}, Required: 2},
	"clamp_max": {Args: []ArgKind{ArgExpr, ArgNumber}, Required: 2},

	// Across buckets.
	"moving_avg": {Args: []ArgKind{ArgExpr, ArgNumber}, Required: 2},
	// diff is the per-bucket change, for a gauge that reports a running
	// total — the shape an exporter produces when it has no counter type.
	"diff": {Args: []ArgKind{ArgExpr}, Required: 1},
	// timeshift moves a query's window by a number of seconds, so last
	// week's line can be drawn under this week's. The offset is normally
	// negative; positive is allowed and means the future, which is empty.
	"timeshift": {Args: []ArgKind{ArgExpr, ArgNumber}, Required: 2},

	// Across series.
	"top": {Args: []ArgKind{ArgExpr, ArgNumber, ArgReducer, ArgOrder}, Required: 2},

	// histogram_quantile(q, <bucket query> by {upper_bound,…}) interpolates a
	// quantile from Prometheus-style cumulative buckets. It is the path for
	// metrics scraped from an exporter, where the buckets are all there is;
	// the pXX aggregators are the path for metrics that arrive as sketches.
	// Having both lets the same data be measured two ways.
	"histogram_quantile": {Args: []ArgKind{ArgNumber, ArgExpr}, Required: 2},
}

// Lookup returns a function's signature. It is how the parser and the
// evaluator ask the one table the same question.
func Lookup(name string) (Signature, bool) {
	sig, ok := functions[name]
	return sig, ok
}

// FunctionNames lists the functions in sorted order, for error messages and
// for the query editor's completion list — which is the reason this exists
// rather than the editor keeping a second list that drifts.
func FunctionNames() []string {
	names := make([]string, 0, len(functions))
	for name := range functions {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

// sortStrings is an insertion sort: the list is a dozen entries and this
// keeps the package's imports to what it really needs.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func oneOf(s string, set []string) bool {
	for _, v := range set {
		if s == v {
			return true
		}
	}
	return false
}

// quoteList renders a set of allowed words for an error message, quoted so
// that a message about a string argument shows the quotes the query needs.
func quoteList(set []string) string {
	out := make([]string, len(set))
	for i, v := range set {
		out[i] = strconv.Quote(v)
	}
	return strings.Join(out, ", ")
}

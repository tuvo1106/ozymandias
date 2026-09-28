package metricql

import (
	"slices"
	"strings"
)

// Walk calls fn on n and on every node beneath it, parents before children.
//
// It exists because more than one thing needs to ask a question of a whole
// expression rather than of one node: the planner looks for the `.rollup()`
// that decides the bucket width, [Variables] looks for template variables, and
// a dashboard's validator wants to know what a widget will ask for before it
// is saved. Each of those wrote its own three-case type switch once, and three
// copies of a traversal is three places to forget a node kind the day the
// grammar grows one.
//
// A leaf — a [Number], a [String], or a [Query] — has no children *for this
// purpose*: a query's filter and modifiers are not Nodes, so reaching into
// them is the caller's business (see [Variables], which does).
func Walk(n Node, fn func(Node)) {
	if n == nil {
		return
	}
	fn(n)
	switch v := n.(type) {
	case *Unary:
		Walk(v.X, fn)
	case *Binary:
		Walk(v.Left, fn)
		Walk(v.Right, fn)
	case *Call:
		for _, a := range v.Args {
			Walk(a, fn)
		}
	}
}

// Variables returns the template variables an expression references, without
// the `$`, lower-cased, deduplicated and sorted.
//
// This is what lets a dashboard be checked rather than trusted: a widget whose
// query says `$env` needs the dashboard to declare `env`, and finding that out
// when the definition is saved is much better than finding out when somebody
// opens the dashboard and every chart is an error. The UI uses the same list
// to decide which selectors to draw.
//
// Lower-cased because the lexer lower-cases a variable name as it reads one,
// so `$Env` and `$env` are the same variable and a declaration of either
// satisfies both.
func Variables(n Node) []string {
	var out []string
	Walk(n, func(node Node) {
		q, ok := node.(*Query)
		if !ok {
			return
		}
		for _, m := range q.Filter {
			if m.Var != "" {
				out = append(out, strings.ToLower(m.Var))
			}
		}
	})
	slices.Sort(out)
	return slices.Compact(out)
}

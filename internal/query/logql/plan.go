package logql

// StreamLabels are the labels the store indexes: a stream is a unique
// combination of them (docs/plan/M4-logs.md §3). Low cardinality by
// construction, which is why status is one of them and trace_id is not.
var StreamLabels = []string{"service", "source", "host", "env", "status"}

func isStreamLabel(k string) bool {
	for _, s := range StreamLabels {
		if s == k {
			return true
		}
	}
	return false
}

// Matcher is one condition on a stream label, answered by the index without
// reading a log: "which streams have service=api and status in error*".
type Matcher struct {
	Key, Value string // Value may contain `*`
	Negate     bool
}

// Matches reports whether a stream whose label has this value satisfies m,
// with a missing label being "" (see [MatchLabel]).
func (m Matcher) Matches(value string) bool { return MatchLabel(m.Value, value) != m.Negate }

// Branch is one way a query can be satisfied: streams that pass every
// Matcher, then logs in them that pass Filter. A nil Filter means the matchers
// were the whole query.
type Branch struct {
	Matchers []Matcher
	Filter   Node
}

// Plan is a query split into what the index answers and what a scan must
// check. A top-level OR is a union of branches, each with its own matchers;
// that is how `service:api OR service:worker` stays an index lookup.
type Plan struct{ Branches []Branch }

// Split plans a query. The matchers are necessary conditions lifted out of
// the top-level conjunction, and Filter is everything else, evaluated on each
// log; a log in a selected stream that passes Filter satisfies the branch,
// which is the property the tests check against a brute-force match.
//
// Only a top-level OR is distributed into branches. An OR nested inside an AND
// (`service:api (status:error OR status:critical)`) stays in the filter and
// costs a scan of every selected stream. Distributing it would multiply
// branches with each nested OR, and the filter already evaluates labels
// correctly; the price is paid only by queries that nest.
func Split(n Node) Plan {
	if n == nil {
		return Plan{Branches: []Branch{{}}}
	}
	var branches []Branch
	for _, b := range orArms(n) {
		branches = append(branches, planBranch(b))
	}
	return Plan{Branches: branches}
}

// orArms flattens a top-level OR (including an OR written inside another).
func orArms(n Node) []Node {
	o, ok := n.(Or)
	if !ok {
		return []Node{n}
	}
	var out []Node
	for _, a := range o.Args {
		out = append(out, orArms(a)...)
	}
	return out
}

func planBranch(n Node) Branch {
	var b Branch
	var rest []Node
	for _, c := range conjuncts(n) {
		if m, ok := asMatcher(c); ok {
			b.Matchers = append(b.Matchers, m)
			continue
		}
		rest = append(rest, c)
	}
	switch len(rest) {
	case 0:
	case 1:
		b.Filter = rest[0]
	default:
		b.Filter = And{Args: rest}
	}
	return b
}

// conjuncts flattens nested ANDs: (a b) c is a b c for matching purposes.
func conjuncts(n Node) []Node {
	a, ok := n.(And)
	if !ok {
		return []Node{n}
	}
	var out []Node
	for _, x := range a.Args {
		out = append(out, conjuncts(x)...)
	}
	return out
}

func asMatcher(n Node) (Matcher, bool) {
	switch n := n.(type) {
	case Label:
		if isStreamLabel(n.Key) {
			return Matcher{Key: n.Key, Value: n.Value}, true
		}
	case Not:
		if l, ok := n.X.(Label); ok && isStreamLabel(l.Key) {
			return Matcher{Key: l.Key, Value: l.Value, Negate: true}, true
		}
	}
	return Matcher{}, false
}

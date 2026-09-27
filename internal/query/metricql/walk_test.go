package metricql

import (
	"slices"
	"testing"
)

// Walk has to reach every node, because its callers use it to decide things
// about a whole expression: which rollup sets the bucket width, which
// variables a dashboard must declare. A node kind it silently skips is a
// question answered wrongly rather than loudly.
func TestWalk_ReachesEveryNode(t *testing.T) {
	expr, err := Parse(`-abs(sum:a{*} + max:b{*} / 2) * 3`)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	Walk(expr, func(n Node) { seen = append(seen, n.String()) })
	// Parents before children, and nothing left out: two queries, three
	// numbers, the call, the negation and the two operators.
	for _, want := range []string{"sum:a{*}", "max:b{*}", "2", "3", "abs(sum:a{*} + max:b{*} / 2)"} {
		if !slices.Contains(seen, want) {
			t.Errorf("Walk never visited %q; saw %v", want, seen)
		}
	}
	if seen[0] != expr.String() {
		t.Errorf("first visit was %q, want the root %q — parents come first", seen[0], expr)
	}
}

func TestWalk_NilIsNotAPanic(t *testing.T) {
	calls := 0
	Walk(nil, func(Node) { calls++ })
	if calls != 0 {
		t.Errorf("visited %d nodes of a nil expression", calls)
	}
}

func TestVariables(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{`sum:a{*}`, nil},
		{`sum:a{$env}`, []string{"env"}},
		{`sum:a{$env,$env}`, []string{"env"}},
		{`sum:a{$Env}`, []string{"env"}},
		// Both sides of arithmetic, and inside a function's arguments.
		{`sum:a{$env} / sum:b{$region}`, []string{"env", "region"}},
		{`top(sum:a{$env,service:x} by {route}, 5, "mean")`, []string{"env"}},
		{`-abs(sum:a{$b} + sum:c{$a})`, []string{"a", "b"}},
		// A variable is a matcher, not a value: `k:$v` is the literal "$v".
		{`sum:a{k:$v}`, nil},
	} {
		t.Run(tc.query, func(t *testing.T) {
			expr, err := Parse(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			got := Variables(expr)
			if !slices.Equal(got, tc.want) {
				t.Errorf("Variables(%s) = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

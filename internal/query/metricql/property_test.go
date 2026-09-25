package metricql

import (
	"math"
	"reflect"
	"testing"

	"pgregory.net/rapid"
)

// The round trip is the property that makes the printer trustworthy: the
// canonical text of a query means exactly the query it was printed from.
// Everything downstream leans on it — the API echoes a normalized "query",
// dashboards are diffed as text, and the fuzz target below compares a parse
// against a re-parse rather than against an expectation written by hand.
//
// Generating ASTs rather than text is what makes it a real test: a generator
// of text can only produce queries someone thought to write, while this one
// produces trees the parser would have to be wrong to reject.
func TestProperty_PrintThenParseIsIdentity(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		want := nodeGen(0).Draw(t, "ast")
		text := want.String()
		got, err := Parse(text)
		if err != nil {
			t.Fatalf("Parse(%q): %v", text, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round trip changed the tree\n text %q\n  got %#v\n want %#v", text, got, want)
		}
		if got.String() != text {
			t.Fatalf("printing is not stable: %q then %q", text, got.String())
		}
	})
}

// A parsed query prints the same however it was spaced or capitalised, which
// is what lets two dashboards be compared without normalising them first.
func TestProperty_PrintingIsIdempotentOverText(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		n := nodeGen(0).Draw(t, "ast")
		once, err := Parse(n.String())
		if err != nil {
			t.Fatalf("Parse(%q): %v", n, err)
		}
		twice, err := Parse(once.String())
		if err != nil {
			t.Fatalf("reparse(%q): %v", once, err)
		}
		if once.String() != twice.String() {
			t.Fatalf("%q then %q", once, twice)
		}
	})
}

// nodeGen draws an expression. depth bounds the recursion well inside
// MaxDepth so that a generated tree is always one the parser accepts — the
// property under test is the round trip, not the depth limit, which has its
// own test.
func nodeGen(depth int) *rapid.Generator[Node] {
	return rapid.Custom(func(t *rapid.T) Node {
		kind := 0
		if depth < 3 {
			kind = rapid.IntRange(0, 3).Draw(t, "kind")
		}
		switch kind {
		case 1:
			return &Binary{
				Op:    rapid.SampledFrom([]Op{OpAdd, OpSub, OpMul, OpDiv}).Draw(t, "op"),
				Left:  nodeGen(depth+1).Draw(t, "left"),
				Right: nodeGen(depth+1).Draw(t, "right"),
			}
		case 2:
			return &Unary{Op: OpSub, X: nodeGen(depth+1).Draw(t, "x")}
		case 3:
			return callGen(depth).Draw(t, "call")
		}
		if rapid.Bool().Draw(t, "isNumber") {
			return numberGen().Draw(t, "number")
		}
		return queryGen().Draw(t, "query")
	})
}

// numberGen draws only non-negative finite values, because those are the only
// ones the parser can produce: a '-' is always its own [Unary] node, so a
// Number holding -1 is a tree no source text spells.
func numberGen() *rapid.Generator[Node] {
	return rapid.Custom(func(t *rapid.T) Node {
		v := rapid.Float64Range(0, math.MaxFloat64).Draw(t, "v")
		if math.Signbit(v) {
			v = 0 // negative zero prints as "-0", which is a Unary
		}
		return &Number{Value: v}
	})
}

func callGen(depth int) *rapid.Generator[Node] {
	return rapid.Custom(func(t *rapid.T) Node {
		name := rapid.SampledFrom(FunctionNames()).Draw(t, "func")
		sig := Functions[name]
		n := rapid.IntRange(sig.Required, len(sig.Args)).Draw(t, "argc")
		c := &Call{Func: name}
		for i := range n {
			switch sig.Args[i] {
			case ArgNumber:
				num := numberGen().Draw(t, "num")
				if rapid.Bool().Draw(t, "negated") {
					c.Args = append(c.Args, &Unary{Op: OpSub, X: num})
					continue
				}
				c.Args = append(c.Args, num)
			case ArgReducer, ArgOrder:
				c.Args = append(c.Args, &String{Value: rapid.SampledFrom(sig.Args[i].values()).Draw(t, "enum")})
			default:
				c.Args = append(c.Args, nodeGen(depth+1).Draw(t, "arg"))
			}
		}
		return c
	})
}

func queryGen() *rapid.Generator[*Query] {
	return rapid.Custom(func(t *rapid.T) *Query {
		q := &Query{
			Agg:    rapid.SampledFrom([]Agg{Avg, Sum, Min, Max, Count, P50, P75, P90, P95, P99}).Draw(t, "agg"),
			Metric: metricGen().Draw(t, "metric"),
		}
		for range rapid.IntRange(0, 3).Draw(t, "matchers") {
			q.Filter = append(q.Filter, matcherGen().Draw(t, "matcher"))
		}
		q.By = distinctKeys(t, rapid.IntRange(0, 3).Draw(t, "bys"))
		q.Modifiers = modifiersGen().Draw(t, "modifiers")
		return q
	})
}

func matcherGen() *rapid.Generator[Matcher] {
	return rapid.Custom(func(t *rapid.T) Matcher {
		switch rapid.IntRange(0, 2).Draw(t, "shape") {
		case 0:
			return Matcher{Var: metricGen().Draw(t, "var")}
		case 1:
			return Matcher{
				Neg:    rapid.Bool().Draw(t, "neg"),
				Key:    keyGen().Draw(t, "key"),
				Values: []string{valueGen().Draw(t, "value")},
			}
		default:
			n := rapid.IntRange(1, 4).Draw(t, "values")
			m := Matcher{Neg: rapid.Bool().Draw(t, "neg"), Key: keyGen().Draw(t, "key"), In: true}
			for range n {
				m.Values = append(m.Values, valueGen().Draw(t, "value"))
			}
			return m
		}
	})
}

func modifiersGen() *rapid.Generator[[]Modifier] {
	return rapid.Custom(func(t *rapid.T) []Modifier {
		var out []Modifier
		add := func(m Modifier) {
			if rapid.Bool().Draw(t, "present") {
				out = append(out, m)
			}
		}
		add(Modifier{
			Kind:    ModRollup,
			Method:  rapid.SampledFrom(rollupMethods).Draw(t, "method"),
			Seconds: rapid.SampledFrom([]int64{0, 1, 60, MaxSeconds}).Draw(t, "secs"),
		})
		if rapid.Bool().Draw(t, "rate") {
			add(Modifier{Kind: ModAsRate})
		} else {
			add(Modifier{Kind: ModAsCount})
		}
		add(Modifier{
			Kind:    ModFill,
			Method:  rapid.SampledFrom(fillModes).Draw(t, "mode"),
			Seconds: rapid.SampledFrom([]int64{0, 30, 300}).Draw(t, "secs"),
		})
		return out
	})
}

// metricGen draws a name the lexer reads as one identifier.
func metricGen() *rapid.Generator[string] {
	return rapid.StringMatching(`[a-z][a-z0-9_.]{0,12}`)
}

func keyGen() *rapid.Generator[string] {
	return rapid.StringMatching(`[a-z][a-z0-9_./-]{0,12}`)
}

// valueGen draws free text, minus the three characters that end a value and
// the whitespace that would be trimmed off one.
func valueGen() *rapid.Generator[string] {
	return rapid.StringMatching(`[a-zA-Z0-9_./:*+!=-]{1,16}`)
}

func distinctKeys(t *rapid.T, n int) []string {
	var out []string
	for range n {
		k := keyGen().Draw(t, "by")
		if !oneOf(k, out) {
			out = append(out, k)
		}
	}
	return out
}

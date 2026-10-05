package logql

import (
	"math"
	"reflect"
	"testing"

	"pgregory.net/rapid"
)

// The round trip is what makes the printer trustworthy (the API echoes a
// normalized query, saved views compare as text), and generating trees rather
// than text is what makes it a real test: the generator produces shapes the
// parser would have to be wrong to reject, including the awkward strings that
// force quoting.
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

// awkward draws strings that exercise quoting: syntax characters, keywords,
// wildcards, escapes, and non-ASCII.
func awkward(t *rapid.T, label string) string {
	return rapid.OneOf(
		rapid.StringOfN(rapid.RuneFrom([]rune{'a', 'b', 'Z', '0', '9', '-', '_', '.', '/', '*'}), 0, 8, -1),
		rapid.StringOfN(rapid.RuneFrom([]rune{'a', ' ', '"', '\\', '(', ')', ':', '@', '-', '>', '<', '[', ']', '\n', '\t', 'é', '界'}), 0, 8, -1),
		rapid.SampledFrom([]string{"AND", "OR", "NOT", "ORDER", "-x", "@x", "a:b", ">5", "<5", "<=5", ">=5", "[1 TO 2]", "[", "[abc", "", "*", " "}),
	).Draw(t, label)
}

func nodeGen(depth int) *rapid.Generator[Node] {
	leaf := rapid.Custom(func(t *rapid.T) Node {
		switch rapid.IntRange(0, 2).Draw(t, "leaf") {
		case 0:
			return Text{Value: awkward(t, "text")}
		case 1:
			return Label{Key: rapid.SampledFrom(ReservedKeys).Draw(t, "key"), Value: awkward(t, "lv")}
		}
		return attrGen().Draw(t, "attr")
	})
	if depth >= 3 {
		return leaf
	}
	return rapid.Custom(func(t *rapid.T) Node {
		switch rapid.IntRange(0, 5).Draw(t, "kind") {
		case 0:
			return Not{X: nodeGen(depth+1).Draw(t, "not")}
		case 1:
			return Or{Args: rapid.SliceOfN(nodeGen(depth+1), 2, 3).Draw(t, "or")}
		case 2:
			return And{Args: rapid.SliceOfN(nodeGen(depth+1), 2, 3).Draw(t, "and")}
		}
		return leaf.Draw(t, "leaf")
	})
}

func attrGen() *rapid.Generator[Node] {
	return rapid.Custom(func(t *rapid.T) Node {
		path := rapid.StringMatching(`[a-zA-Z_][a-zA-Z0-9_.-]{0,8}`).Draw(t, "path")
		finite := rapid.Float64().Filter(func(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) })
		switch op := Op(rapid.IntRange(0, 5).Draw(t, "op")); op {
		case OpEq:
			return Attr{Path: path, Op: OpEq, Value: awkward(t, "av")}
		case OpRange:
			a, b := finite.Draw(t, "lo"), finite.Draw(t, "hi")
			if a > b {
				a, b = b, a
			}
			return Attr{Path: path, Op: OpRange, Num: a, Hi: b}
		default:
			return Attr{Path: path, Op: op, Num: finite.Draw(t, "num")}
		}
	})
}

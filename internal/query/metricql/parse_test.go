package metricql

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// q is a terse constructor for the expected ASTs below, so a table row is
// readable as the query it stands for.
func q(agg Agg, metric string, f Filter, by []string, mods ...Modifier) *Query {
	return &Query{Agg: agg, Metric: metric, Filter: f, By: by, Modifiers: mods}
}

func eq(key, value string) Matcher { return Matcher{Key: key, Values: []string{value}} }

func TestParse_Productions(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      Node
	}{
		{
			"the simplest query",
			"avg:x{*}",
			q(Avg, "x", nil, nil),
		},
		{
			"a dotted metric and several matchers",
			"sum:http.request.count{service:api,env:dev}",
			q(Sum, "http.request.count", Filter{eq("service", "api"), eq("env", "dev")}, nil),
		},
		{
			"a negated matcher",
			"max:x{!env:prod}",
			q(Max, "x", Filter{{Key: "env", Neg: true, Values: []string{"prod"}}}, nil),
		},
		{
			"a wildcard value stays text",
			"count:x{status:5*}",
			q(Count, "x", Filter{eq("status", "5*")}, nil),
		},
		{
			"a value with slashes and colons",
			"avg:x{route:/api/items,url:http://h:1/p}",
			q(Avg, "x", Filter{eq("route", "/api/items"), eq("url", "http://h:1/p")}, nil),
		},
		{
			// A value is delimited by what follows it and by nothing else, so
			// characters that are operators or errors in an expression are
			// ordinary text here. This is what the lexer's modes buy.
			"a value made of characters the expression grammar reserves",
			"avg:x{v:a;b-c*d!(e)}",
			q(Avg, "x", Filter{eq("v", "a;b-c*d!(e)")}, nil),
		},
		{
			"an IN list",
			"sum:x{status IN (500,502,503)}",
			q(Sum, "x", Filter{{Key: "status", In: true, Values: []string{"500", "502", "503"}}}, nil),
		},
		{
			"a negated IN list, spelled in lower case",
			"sum:x{!status in (a, b)}",
			q(Sum, "x", Filter{{Key: "status", Neg: true, In: true, Values: []string{"a", "b"}}}, nil),
		},
		{
			"a template variable",
			"sum:x{$env,service:api}",
			q(Sum, "x", Filter{{Var: "env"}, eq("service", "api")}, nil),
		},
		{
			"group by keys that contain operators",
			"p95:x{*} by {route,status-code,a/b}",
			q(P95, "x", nil, []string{"route", "status-code", "a/b"}),
		},
		{
			"every modifier at once",
			"sum:x{*}.rollup(max, 60).as_rate().fill(linear, 300)",
			q(Sum, "x", nil, nil,
				Modifier{Kind: ModRollup, Method: "max", Seconds: 60},
				Modifier{Kind: ModAsRate},
				Modifier{Kind: ModFill, Method: "linear", Seconds: 300}),
		},
		{
			"rollup without a width",
			"sum:x{*}.rollup(sum)",
			q(Sum, "x", nil, nil, Modifier{Kind: ModRollup, Method: "sum"}),
		},
		{
			"as_count",
			"sum:x{*}.as_count()",
			q(Sum, "x", nil, nil, Modifier{Kind: ModAsCount}),
		},
		{
			"arithmetic, left-associative",
			"avg:a{*} - avg:b{*} - avg:c{*}",
			&Binary{Op: OpSub,
				Left:  &Binary{Op: OpSub, Left: q(Avg, "a", nil, nil), Right: q(Avg, "b", nil, nil)},
				Right: q(Avg, "c", nil, nil)},
		},
		{
			"multiplication binds tighter than addition",
			"1 + 2 * 3",
			&Binary{Op: OpAdd, Left: &Number{1},
				Right: &Binary{Op: OpMul, Left: &Number{2}, Right: &Number{3}}},
		},
		{
			"parentheses override it",
			"(1 + 2) * 3",
			&Binary{Op: OpMul,
				Left:  &Binary{Op: OpAdd, Left: &Number{1}, Right: &Number{2}},
				Right: &Number{3}},
		},
		{
			"negation",
			"-avg:x{*}",
			&Unary{Op: OpSub, X: q(Avg, "x", nil, nil)},
		},
		{
			"subtracting a negative",
			"1 - -2",
			&Binary{Op: OpSub, Left: &Number{1}, Right: &Unary{Op: OpSub, X: &Number{2}}},
		},
		{
			"a function of a query",
			"abs(avg:x{*})",
			&Call{Func: "abs", Args: []Node{q(Avg, "x", nil, nil)}},
		},
		{
			"a function with every argument kind",
			`top(sum:x{*} by {k}, 5, "mean", "desc")`,
			&Call{Func: "top", Args: []Node{
				q(Sum, "x", nil, []string{"k"}), &Number{5}, &String{"mean"}, &String{"desc"},
			}},
		},
		{
			"optional arguments may be left off",
			"top(sum:x{*}, 5)",
			&Call{Func: "top", Args: []Node{q(Sum, "x", nil, nil), &Number{5}}},
		},
		{
			"a negative number argument",
			"timeshift(avg:x{*}, -3600)",
			&Call{Func: "timeshift", Args: []Node{
				q(Avg, "x", nil, nil), &Unary{Op: OpSub, X: &Number{3600}},
			}},
		},
		{
			"nested calls",
			"clamp_min(abs(avg:x{*}), 0)",
			&Call{Func: "clamp_min", Args: []Node{
				&Call{Func: "abs", Args: []Node{q(Avg, "x", nil, nil)}}, &Number{0},
			}},
		},
		{
			"the ratio from the milestone spec",
			"sum:h.c{status:5*}.as_rate() / sum:h.c{*}.as_rate() * 100",
			&Binary{Op: OpMul,
				Left: &Binary{Op: OpDiv,
					Left:  q(Sum, "h.c", Filter{eq("status", "5*")}, nil, Modifier{Kind: ModAsRate}),
					Right: q(Sum, "h.c", nil, nil, Modifier{Kind: ModAsRate})},
				Right: &Number{100}},
		},
		{
			"space is not significant between tokens",
			"  sum : x { a : b }  by  { k } . as_rate ( ) ",
			q(Sum, "x", Filter{eq("a", "b")}, []string{"k"}, Modifier{Kind: ModAsRate}),
		},
		{
			"an aggregator typed in capitals",
			"SUM:x{*}",
			q(Sum, "x", nil, nil),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.src, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Parse(%q)\n got %s\nwant %s\n (%#v)", tc.src, got, tc.want, got)
			}
			// Every production must survive a trip through the printer.
			back, err := Parse(got.String())
			if err != nil {
				t.Fatalf("reparsing %q: %v", got, err)
			}
			if !reflect.DeepEqual(back, got) {
				t.Errorf("print/parse changed the tree:\n %s\n %s", got, back)
			}
		})
	}
}

// The column is the whole point of these errors: the query editor underlines
// it, so a message that points one token too far is worse than no column.
func TestParse_Errors(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		col       int
		contains  string
	}{
		{"empty", "", 1, "expected a query"},
		{"only a name", "foo", 1, "is not a query or a function call"},
		{"unknown aggregator", "median:x{*}", 1, "unknown aggregator"},
		{"no metric", "sum:{*}", 5, "expected a metric name"},
		{"a metric that is not one", "sum:9x{*}", 5, "expected a metric name"},
		{"no filter at all", "sum:x", 6, "expected '{'"},
		{"an empty filter", "sum:x{}", 7, "write {*}"},
		{"unclosed filter", "sum:x{a:b", 10, "expected ',' or '}'"},
		{"a missing value", "sum:x{env:}", 11, "expected a value"},
		{"a missing value in a list", "sum:x{env IN (a,)}", 17, "expected a value"},
		{"a key with no operator", "sum:x{env}", 10, "expected ':' or ' IN ('"},
		{"a negated variable", "sum:x{!$env}", 7, "cannot be negated"},
		{"junk where a key goes", "sum:x{1:2}", 7, "expected a tag key"},
		{"by with no brace", "sum:x{*} by k", 13, "expected '{'"},
		{"by with no key", "sum:x{*} by {}", 14, "expected a tag key"},
		{"a repeated group-by key", "sum:x{*} by {k,k}", 16, "already in the group-by"},
		{"an unknown modifier", "sum:x{*}.foo()", 10, "unknown modifier"},
		{"a repeated modifier", "sum:x{*}.as_rate().as_rate()", 20, "already set"},
		{"a bad rollup method", "sum:x{*}.rollup(median)", 17, `expected a rollup method ("avg", "sum"`},
		{"a bad fill mode", "sum:x{*}.fill(zeroes)", 15, "expected a fill mode"},
		{"arguments to as_rate", "sum:x{*}.as_rate(60)", 18, "takes no arguments"},
		{"a fractional rollup width", "sum:x{*}.rollup(avg, 1.5)", 22, "whole number of seconds"},
		{"a zero rollup width", "sum:x{*}.rollup(avg, 0)", 22, "whole number of seconds"},
		{"an unknown function", "nope(sum:x{*})", 1, "unknown function"},
		{"too few arguments", "clamp_min(sum:x{*})", 1, "takes 2 arguments"},
		{"too many arguments", "abs(sum:x{*}, 1)", 15, "takes at most 1 argument"},
		{"an expression where a number goes", "clamp_min(sum:x{*}, 1+1)", 21, "must be a plain number"},
		{"an unquoted enum argument", `top(sum:x{*}, 5, mean, "desc")`, 18, "must be one of"},
		{"a word outside the enum", `top(sum:x{*}, 5, "median", "desc")`, 18, "must be one of"},
		{"a string on its own", `"x"`, 1, "only allowed as a function argument"},
		{"trailing junk", "sum:x{*} sum:y{*}", 10, "after the end of the query"},
		{"an unclosed paren", "(1 + 2", 7, "expected ')'"},
		{"a dangling operator", "1 +", 4, "expected a query, a number or '('"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.src)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded", tc.src)
			}
			var perr *Error
			if !errors.As(err, &perr) {
				t.Fatalf("got %T (%v), want *Error", err, err)
			}
			if perr.Col != tc.col {
				t.Errorf("col %d, want %d — %v", perr.Col, tc.col, err)
			}
			if !strings.Contains(perr.Msg, tc.contains) {
				t.Errorf("message %q does not mention %q", perr.Msg, tc.contains)
			}
		})
	}
}

// Limits are part of the parser's contract: it runs on whatever arrives at
// the API, before anything has decided the request is reasonable.
func TestParse_Limits(t *testing.T) {
	t.Run("length", func(t *testing.T) {
		src := "sum:x{env:" + strings.Repeat("a", MaxLen) + "}"
		if _, err := Parse(src); err == nil || !strings.Contains(err.Error(), "over the") {
			t.Errorf("got %v, want a length error", err)
		}
	})
	t.Run("depth", func(t *testing.T) {
		src := strings.Repeat("(", 200) + "1" + strings.Repeat(")", 200)
		_, err := Parse(src)
		if err == nil || !strings.Contains(err.Error(), "nests more than") {
			t.Errorf("got %v, want a depth error", err)
		}
	})
	t.Run("depth through negation", func(t *testing.T) {
		// Unary recurses into factor without passing through expr, so it
		// needs its own counting or it is a way around the limit.
		src := strings.Repeat("-", 5000) + "1"
		if _, err := Parse(src); err == nil || !strings.Contains(err.Error(), "nests more than") {
			t.Errorf("got %v, want a depth error", err)
		}
	})
	t.Run("values in an IN list", func(t *testing.T) {
		src := "sum:x{k IN (" + strings.Repeat("a,", MaxValues) + "b)}"
		if _, err := Parse(src); err == nil || !strings.Contains(err.Error(), "values in one IN list") {
			t.Errorf("got %v, want an IN-list error", err)
		}
	})
	t.Run("a tag that could never be stored", func(t *testing.T) {
		src := "sum:x{k:" + strings.Repeat("v", 300) + "}"
		if _, err := Parse(src); err == nil || !strings.Contains(err.Error(), "not a valid tag value") {
			t.Errorf("got %v, want a tag-length error", err)
		}
	})
}

// Quantile is how the planner decides a query needs the sketch store.
func TestAgg_Quantile(t *testing.T) {
	for _, tc := range []struct {
		agg  Agg
		want float64
		ok   bool
	}{
		{P50, 0.5, true}, {P75, 0.75, true}, {P90, 0.9, true},
		{P95, 0.95, true}, {P99, 0.99, true},
		{Avg, 0, false}, {Sum, 0, false}, {Min, 0, false},
		{Max, 0, false}, {Count, 0, false}, {Agg("p42"), 0, false},
	} {
		got, ok := tc.agg.Quantile()
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got (%v, %v), want (%v, %v)", tc.agg, got, ok, tc.want, tc.ok)
		}
	}
}

func TestFunctionNames_AreSortedAndComplete(t *testing.T) {
	names := FunctionNames()
	if len(names) != len(Functions) {
		t.Fatalf("listed %d of %d functions", len(names), len(Functions))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("not sorted at %d: %q then %q", i, names[i-1], names[i])
		}
	}
}

// A lexical error can happen anywhere the parser asks for a token, and each
// of those places has to pass it up rather than swallow it or report it at
// the wrong place. This walks every one of them: the column in each row is
// the ';', which is not a character the language has anywhere.
func TestParse_LexicalErrorsEverywhere(t *testing.T) {
	for _, src := range []string{
		";",
		"1 + ;",
		"1 * ;",
		"-;",
		"(;",
		"sum;",
		"sum:;",
		"sum:x;",
		"sum:x{;}",
		"sum:x{*};",
		"sum:x{!;}",
		"sum:x{$e;}",
		"sum:x{a;}",
		"sum:x{a IN;}",
		"sum:x{*} by;",
		"sum:x{*} by {;}",
		"sum:x{*} by {k;}",
		"sum:x{*}.;",
		"sum:x{*}.as_rate;",
		"sum:x{*}.as_rate(;",
		"sum:x{*}.rollup(avg, 60;",
		"sum:x{*}.rollup(;",
		"abs(;",
		"abs(1;",
		"top(sum:x{*},;",
		`top(sum:x{*}, 5, "mean";`,
	} {
		t.Run(src, func(t *testing.T) {
			_, err := Parse(src)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded", src)
			}
			var perr *Error
			if !errors.As(err, &perr) {
				t.Fatalf("got %T, want *Error", err)
			}
			want := strings.IndexByte(src, ';') + 1
			if perr.Col != want {
				t.Errorf("col %d, want %d (the ';') — %v", perr.Col, want, err)
			}
		})
	}
}

// Forgetting the closing brace is the commonest way to mistype a query, and
// a tag value that ran to the end of the line would swallow the rest of it
// and parse — wrong, and silent. The value stops at a brace so that it does
// not.
func TestParse_AnUnclosedFilterIsAnError(t *testing.T) {
	for _, src := range []string{
		"avg:x{service:api by {route}",
		"avg:x{service:api",
		"avg:x{a:b,c:d",
	} {
		if n, err := Parse(src); err == nil {
			t.Errorf("Parse(%q) returned %s, want an error", src, n)
		}
	}
}

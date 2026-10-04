package logql

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParse_Valid(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Node
		text string // canonical print; "" means same as in
	}{
		{"error", Text{"error"}, ""},
		{"  error  ", Text{"error"}, "error"},
		{`"connection refused"`, Text{"connection refused"}, ""},
		{"err*", Text{"err*"}, ""},
		{"*", Text{"*"}, ""},
		{"service:api", Label{"service", "api"}, ""},
		{`service:"my api"`, Label{"service", "my api"}, ""},
		{"service:app-*", Label{"service", "app-*"}, ""},
		{"trace_id:abc", Label{"trace_id", "abc"}, ""},
		{"@tag:api", Attr{Path: "tag", Value: "api"}, ""},
		{"@http.status:200", Attr{Path: "http.status", Value: "200"}, ""},
		{"@path:/api/comics", Attr{Path: "path", Value: "/api/comics"}, ""},
		{"@ms:>200", Attr{Path: "ms", Op: OpGt, Num: 200}, ""},
		{"@ms:>=200", Attr{Path: "ms", Op: OpGe, Num: 200}, ""},
		{"@ms:<1.5", Attr{Path: "ms", Op: OpLt, Num: 1.5}, ""},
		{"@ms:<=-3", Attr{Path: "ms", Op: OpLe, Num: -3}, ""},
		{"@ms:[100 TO 500]", Attr{Path: "ms", Op: OpRange, Num: 100, Hi: 500}, ""},
		{"@ms:[ 100   TO 500 ]", Attr{Path: "ms", Op: OpRange, Num: 100, Hi: 500}, "@ms:[100 TO 500]"},
		{`@msg:">5"`, Attr{Path: "msg", Value: ">5"}, ""},
		{"@x:*", Attr{Path: "x", Value: "*"}, ""},
		// Juxtaposition and AND are the same node.
		{"a b", And{[]Node{Text{"a"}, Text{"b"}}}, ""},
		{"a AND b", And{[]Node{Text{"a"}, Text{"b"}}}, "a b"},
		{"a b c", And{[]Node{Text{"a"}, Text{"b"}, Text{"c"}}}, ""},
		{"a OR b", Or{[]Node{Text{"a"}, Text{"b"}}}, ""},
		// AND binds tighter than OR.
		{"a b OR c", Or{[]Node{And{[]Node{Text{"a"}, Text{"b"}}}, Text{"c"}}}, ""},
		{"a OR b c", Or{[]Node{Text{"a"}, And{[]Node{Text{"b"}, Text{"c"}}}}}, ""},
		{"(a OR b) c", And{[]Node{Or{[]Node{Text{"a"}, Text{"b"}}}, Text{"c"}}}, ""},
		// Parens are kept as written, so printing puts them back.
		{"a (b c)", And{[]Node{Text{"a"}, And{[]Node{Text{"b"}, Text{"c"}}}}}, ""},
		{"((a))", Text{"a"}, "a"},
		// Negation.
		{"-a", Not{Text{"a"}}, ""},
		{"NOT a", Not{Text{"a"}}, "-a"},
		{"-service:api", Not{Label{"service", "api"}}, ""},
		{"-(a OR b)", Not{Or{[]Node{Text{"a"}, Text{"b"}}}}, ""},
		{"--a", Not{Not{Text{"a"}}}, ""},
		{"a -b", And{[]Node{Text{"a"}, Not{Text{"b"}}}}, ""},
		// A hyphen inside a word is just a hyphen.
		{"web-api", Text{"web-api"}, ""},
		// Quoting what would otherwise be syntax.
		{`"AND"`, Text{"AND"}, ""},
		{`"-x"`, Text{"-x"}, ""},
		{`"a:b"`, Text{"a:b"}, ""},
		{"ORDER", Text{"ORDER"}, ""},
		{"ANDROID", Text{"ANDROID"}, ""},
		{`"say \"hi\""`, Text{`say "hi"`}, ""},
		{`"a\nb"`, Text{"a\nb"}, ""},
		// A colon after a name that is not a reserved key is only an error
		// at the start of a term; later in a word it is text.
		{"a.b:c", Text{"a.b:c"}, `"a.b:c"`},
		// A name may not start with a digit, so this is text, not an unknown key.
		{"9lives:x", Text{"9lives:x"}, `"9lives:x"`},
	} {
		got, err := Parse(c.in)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Parse(%q)\n got  %#v\n want %#v", c.in, got, c.want)
			continue
		}
		want := c.text
		if want == "" {
			want = c.in
		}
		if got.String() != want {
			t.Errorf("Parse(%q).String() = %q, want %q", c.in, got.String(), want)
		}
	}
}

func TestParse_EmptyMatchesEverything(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t"} {
		n, err := Parse(in)
		if err != nil || n != nil {
			t.Errorf("Parse(%q) = %v, %v; want nil, nil", in, n, err)
		}
	}
}

func TestParse_ErrorsPointAtTheProblem(t *testing.T) {
	for _, c := range []struct {
		in   string
		col  int
		frag string
	}{
		{"(a", 1, "never closed"},
		{"a (b", 3, "never closed"},
		{"a)", 2, "unexpected"},
		{")", 1, "unexpected"},
		{"()", 1, "empty parentheses"},
		{"a OR", 3, "after OR"},
		{"a OR )", 3, "after OR"},
		{"a AND", 3, "after AND"},
		{"a AND OR b", 3, "after AND"},
		{"OR a", 1, "before it"},
		{"AND a", 1, "before it"},
		{"a AND AND b", 7, "before it"},
		{"NOT", 1, "after NOT"},
		{"a NOT", 3, "after NOT"},
		{"(NOT)", 2, "after NOT"},
		{"- a", 1, "directly"},
		{"-", 1, "directly"},
		{`"abc`, 1, "unterminated"},
		{`"abc\`, 1, "unterminated"},
		{`"a\qb"`, 4, "unknown escape"},
		{"foo:bar", 1, "unknown key"},
		{"level:error", 1, "unknown key"},
		{"a foo:bar", 3, "unknown key"},
		{"service:", 9, "needs a value"},
		{"service: x", 9, "needs a value"},
		{"service:)", 9, "needs a value"},
		{"@", 1, "attribute name"},
		{"@:x", 1, "attribute name"},
		{"@ms", 4, "needs ':'"},
		{"@ms:", 5, "needs a value"},
		{"@ms:>", 6, "not a number"},
		{"@ms:>abc", 6, "not a number"},
		{"@ms:>=", 7, "not a number"},
		{"@ms:>NaN", 6, "not a number"},
		{"@ms:>1e999", 6, "not a number"},
		{"@ms:[1 TO", 5, "missing its ']'"},
		{"@ms:[1 2]", 5, "[low TO high]"},
		{"@ms:[a TO 2]", 5, "must be numbers"},
		{"@ms:[5 TO 1]", 5, "empty"},
		{"@ms:[1 FROM 3]", 5, "[low TO high]"},
	} {
		_, err := Parse(c.in)
		var pe *Error
		if !errors.As(err, &pe) {
			t.Errorf("Parse(%q): err = %v, want a *Error", c.in, err)
			continue
		}
		if pe.Col != c.col || !strings.Contains(pe.Msg, c.frag) {
			t.Errorf("Parse(%q) = col %d %q; want col %d containing %q", c.in, pe.Col, pe.Msg, c.col, c.frag)
		}
	}
}

func TestParse_UnknownKeyMessageNamesTheFix(t *testing.T) {
	_, err := Parse("level:error")
	msg := err.Error()
	for _, want := range []string{"service", "trace_id", "@level:"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
}

func TestParse_DepthIsBounded(t *testing.T) {
	deep := strings.Repeat("(", MaxDepth+5) + "a" + strings.Repeat(")", MaxDepth+5)
	if _, err := Parse(deep); err == nil || !strings.Contains(err.Error(), "deeper") {
		t.Errorf("deep parens: %v", err)
	}
	if _, err := Parse(strings.Repeat("-", MaxDepth+5) + "a"); err == nil {
		t.Error("deep negation accepted")
	}
	// The boundary is exact: MaxDepth-1 nested levels parse, MaxDepth do not.
	nest := func(k int) string { return strings.Repeat("(", k) + "a" + strings.Repeat(")", k) }
	if _, err := Parse(nest(MaxDepth - 1)); err != nil {
		t.Errorf("a query one inside the limit: %v", err)
	}
	if _, err := Parse(nest(MaxDepth)); err == nil {
		t.Errorf("a query at the limit was accepted")
	}
}

// An attribute value that starts like a comparison or a range must print
// quoted, or reading the text back would parse it as one.
func TestPrint_QuotesWhatWouldReparseAsSyntax(t *testing.T) {
	for _, v := range []string{">5", "<5", "[abc", "[1 TO 2]", "", "a b", `a"b`} {
		n := Attr{Path: "x", Op: OpEq, Value: v}
		got, err := Parse(n.String())
		if err != nil || !reflect.DeepEqual(got, n) {
			t.Errorf("Attr value %q printed as %s and parsed to %#v (%v)", v, n, got, err)
		}
	}
}

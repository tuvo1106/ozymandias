package metricql

import (
	"errors"
	"testing"
)

// The lexer has no public entry point, so these tests drive it the way the
// parser does: one mode per call, chosen by what is expected next.
func TestLexer_ExpressionMode(t *testing.T) {
	l := &lexer{src: `sum:a.b{  } 1.5e-3 "x\"y" $env + - * / ( ) , : . !`}
	want := []struct {
		kind tokenKind
		text string
	}{
		{tokIdent, "sum"}, {tokColon, ":"}, {tokIdent, "a.b"},
		{tokLBrace, "{"}, {tokRBrace, "}"},
		{tokNumber, "1.5e-3"}, {tokString, `x"y`}, {tokVar, "env"},
		{tokPlus, "+"}, {tokMinus, "-"}, {tokStar, "*"}, {tokSlash, "/"},
		{tokLParen, "("}, {tokRParen, ")"}, {tokComma, ","},
		{tokColon, ":"}, {tokDot, "."}, {tokBang, "!"},
		{tokEOF, ""},
	}
	for i, w := range want {
		got, err := l.next()
		if err != nil {
			t.Fatalf("token %d: %v", i, err)
		}
		if got.kind != w.kind || got.text != w.text {
			t.Errorf("token %d: got %v %q, want %v %q", i, got.kind, got.text, w.kind, w.text)
		}
	}
}

// The three modes disagree about the same bytes, which is the whole reason
// they exist: '-' and '/' are operators in an expression and ordinary
// characters in a tag key, and a value is whatever is left.
func TestLexer_ModesDisagreeOnPurpose(t *testing.T) {
	const src = `a-b/c`

	expr := &lexer{src: src}
	if tok, _ := expr.next(); tok.kind != tokIdent || tok.text != "a" {
		t.Errorf("expression mode: got %v %q, want the name a", tok.kind, tok.text)
	}

	key := &lexer{src: src}
	if tok, _ := key.nextKey(); tok.kind != tokKey || tok.text != "a-b/c" {
		t.Errorf("key mode: got %v %q, want the whole key", tok.kind, tok.text)
	}

	value := &lexer{src: src}
	if tok := value.nextValue(",}"); tok.text != "a-b/c" {
		t.Errorf("value mode: got %q, want the whole value", tok.text)
	}
}

func TestLexer_ValueMode(t *testing.T) {
	for _, tc := range []struct {
		name, src, stop, want string
		end                   byte
	}{
		{"a path", "/api/items,x", ",}", "/api/items", ','},
		{"stops at the brace", "prod}", ",}", "prod", '}'},
		{"surrounding space is not part of it", "  a b  ,", ",}", "a b", ','},
		{"empty", "}", ",}", "", '}'},
		{"a url, colons and all", "http://x:8080/p}", ",}", "http://x:8080/p", '}'},
		{"in a list", "a)", ",)", "a", ')'},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &lexer{src: tc.src}
			got := l.nextValue(tc.stop)
			if got.text != tc.want {
				t.Errorf("got %q, want %q", got.text, tc.want)
			}
			if l.src[l.pos] != tc.end {
				t.Errorf("stopped at %q, want %q", l.src[l.pos], tc.end)
			}
		})
	}
}

// A key is lower-cased as it is read, because the intake lower-cases every
// tag it stores: a filter typed in capitals that matched nothing would be a
// silent wrong answer.
func TestLexer_KeysAreLowerCased(t *testing.T) {
	l := &lexer{src: "StatusCode:x"}
	tok, err := l.nextKey()
	if err != nil {
		t.Fatal(err)
	}
	if tok.text != "statuscode" {
		t.Errorf("got %q, want it lower-cased", tok.text)
	}
}

func TestLexer_Numbers(t *testing.T) {
	for _, tc := range []struct {
		src   string
		want  float64
		stops int // how many bytes the number should consume
	}{
		{"0", 0, 1},
		{"42", 42, 2},
		{"1.5", 1.5, 3},
		{"1e3", 1000, 3},
		{"1E+3", 1000, 4},
		{"1e-3", 0.001, 4},
		{"3.", 3, 1},     // the '.' is a modifier dot, not a decimal point
		{"1e", 1, 1},     // and "e" is a name
		{"1ex", 1, 1},    //
		{"0.5x", 0.5, 3}, //
	} {
		t.Run(tc.src, func(t *testing.T) {
			l := &lexer{src: tc.src}
			tok, err := l.next()
			if err != nil {
				t.Fatalf("next: %v", err)
			}
			if tok.kind != tokNumber || tok.num != tc.want {
				t.Errorf("got %v %v, want the number %v", tok.kind, tok.num, tc.want)
			}
			if l.pos != tc.stops {
				t.Errorf("consumed %d bytes, want %d", l.pos, tc.stops)
			}
		})
	}
}

func TestLexer_Errors(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		col       int
	}{
		{"a stray character", "a ; b", 3},
		{"a bare dollar", "$ ", 1},
		{"an unterminated string", `"abc`, 1},
		{"a string across lines", "\"a\nb\"", 1},
		{"an unknown escape", `"a\nb"`, 3},
		{"an escape at the end", `"a\`, 3},
		{"a number too large", "1e999", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := &lexer{src: tc.src}
			var err error
			for i := 0; i < 8 && err == nil; i++ {
				var tok token
				tok, err = l.next()
				if tok.kind == tokEOF {
					break
				}
			}
			var perr *Error
			if err == nil {
				t.Fatalf("no error for %q", tc.src)
			}
			if !errors.As(err, &perr) {
				t.Fatalf("got %T, want *Error", err)
			}
			if perr.Col != tc.col {
				t.Errorf("col %d, want %d (%v)", perr.Col, tc.col, err)
			}
		})
	}
}

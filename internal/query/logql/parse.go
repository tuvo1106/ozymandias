package logql

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// MaxDepth bounds nesting (parentheses and negations). A query is typed by a
// person; a thousand-deep one is a mistake or an attack, and recursion depth
// is the one thing in a recursive-descent parser an input can use to hurt it.
const MaxDepth = 64

// Error is a parse failure with the position the query editor underlines.
type Error struct {
	Msg string
	// Col is the 1-based byte offset the failure points at.
	Col int
}

func (e *Error) Error() string { return fmt.Sprintf("col %d: %s", e.Col, e.Msg) }

// Parse parses a log search. The empty query (or only spaces) is valid and
// returns a nil Node: it matches every log.
//
// The grammar is context-sensitive, so there is no separate lexer. `a:b` is a
// key-value term only when `a` is a reserved key; `-` negates only at the
// start of a term; `>`, `<` and `[` mean "comparison" only right after
// `@path:`. A scanner that did not know where it was would have to quote far
// more than anyone wants to type. What it costs is that every such rule has
// to be said in one place, here.
func Parse(q string) (Node, error) {
	p := &parser{s: q}
	p.skipSpace()
	if p.eof() {
		return nil, nil
	}
	n, err := p.or(0)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if !p.eof() {
		return nil, p.errHere("unexpected %q; a ')' with no '(' before it?", string(p.s[p.i]))
	}
	return n, nil
}

type parser struct {
	s string
	i int
}

func (p *parser) eof() bool { return p.i >= len(p.s) }

func (p *parser) skipSpace() {
	for p.i < len(p.s) && isSpace(p.s[p.i]) {
		p.i++
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func (p *parser) errAt(pos int, format string, a ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, a...), Col: pos + 1}
}

func (p *parser) errHere(format string, a ...any) *Error { return p.errAt(p.i, format, a...) }

// keywordAt reports whether the word at the cursor is exactly kw, standing
// alone (a quoted "OR" and the prefix of `ORDER` are text, not operators).
func (p *parser) keywordAt(kw string) bool {
	if !strings.HasPrefix(p.s[p.i:], kw) {
		return false
	}
	end := p.i + len(kw)
	return end == len(p.s) || isSpace(p.s[end]) || p.s[end] == '(' || p.s[end] == ')'
}

func (p *parser) or(depth int) (Node, error) {
	first, err := p.and(depth)
	if err != nil {
		return nil, err
	}
	args := []Node{first}
	for {
		p.skipSpace()
		if p.eof() || !p.keywordAt("OR") {
			break
		}
		at := p.i
		p.i += 2
		p.skipSpace()
		if p.eof() || p.s[p.i] == ')' {
			return nil, p.errAt(at, "expected a search term after OR")
		}
		next, err := p.and(depth)
		if err != nil {
			return nil, err
		}
		args = append(args, next)
	}
	if len(args) == 1 {
		return args[0], nil
	}
	return Or{Args: args}, nil
}

func (p *parser) and(depth int) (Node, error) {
	first, err := p.unary(depth)
	if err != nil {
		return nil, err
	}
	args := []Node{first}
	for {
		p.skipSpace()
		if p.eof() || p.s[p.i] == ')' || p.keywordAt("OR") {
			break
		}
		if p.keywordAt("AND") {
			at := p.i
			p.i += 3
			p.skipSpace()
			if p.eof() || p.s[p.i] == ')' || p.keywordAt("OR") {
				return nil, p.errAt(at, "expected a search term after AND")
			}
		}
		next, err := p.unary(depth)
		if err != nil {
			return nil, err
		}
		args = append(args, next)
	}
	if len(args) == 1 {
		return args[0], nil
	}
	return And{Args: args}, nil
}

func (p *parser) unary(depth int) (Node, error) {
	p.skipSpace()
	if p.eof() {
		return nil, p.errHere("expected a search term")
	}
	if depth >= MaxDepth {
		return nil, p.errHere("query nests deeper than %d levels", MaxDepth)
	}
	switch {
	case p.keywordAt("OR") || p.keywordAt("AND"):
		kw := "OR"
		if p.keywordAt("AND") {
			kw = "AND"
		}
		return nil, p.errHere("%s needs a search term before it (quote it to search for the word)", kw)
	case p.keywordAt("NOT"):
		at := p.i
		p.i += 3
		p.skipSpace()
		if p.eof() || p.s[p.i] == ')' {
			return nil, p.errAt(at, "expected a search term after NOT")
		}
		x, err := p.unary(depth + 1)
		if err != nil {
			return nil, err
		}
		return Not{X: x}, nil
	case p.s[p.i] == '-':
		p.i++
		if p.eof() || isSpace(p.s[p.i]) || p.s[p.i] == ')' {
			return nil, p.errAt(p.i-1, "a '-' must be followed directly by the term it negates")
		}
		x, err := p.unary(depth + 1)
		if err != nil {
			return nil, err
		}
		return Not{X: x}, nil
	}
	return p.atom(depth)
}

func (p *parser) atom(depth int) (Node, error) {
	switch c := p.s[p.i]; c {
	case '(':
		open := p.i
		p.i++
		p.skipSpace()
		if p.eof() {
			return nil, p.errAt(open, "'(' is never closed")
		}
		if p.s[p.i] == ')' {
			return nil, p.errAt(open, "empty parentheses")
		}
		n, err := p.or(depth + 1)
		if err != nil {
			return nil, err
		}
		p.skipSpace()
		if p.eof() {
			return nil, p.errAt(open, "'(' is never closed")
		}
		p.i++ // the ')' the inner loops stopped at
		return n, nil
	case ')':
		return nil, p.errHere("unexpected ')'")
	case '"':
		s, err := p.quoted()
		if err != nil {
			return nil, err
		}
		return Text{Value: s}, nil
	case '@':
		return p.attr()
	}
	if key, ok := p.keyAt(); ok {
		return p.label(key)
	}
	return Text{Value: p.bare()}, nil
}

// keyAt reports a `name:` at the cursor without consuming it: letters,
// digits and underscores (starting with a letter or underscore), then ':'.
func (p *parser) keyAt() (string, bool) {
	j := p.i
	for j < len(p.s) {
		c := p.s[j]
		if c == '_' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || (j > p.i && '0' <= c && c <= '9') {
			j++
			continue
		}
		break
	}
	if j == p.i || j >= len(p.s) || p.s[j] != ':' {
		return "", false
	}
	return p.s[p.i:j], true
}

func (p *parser) label(key string) (Node, error) {
	at := p.i
	if !isReserved(key) {
		return nil, p.errAt(at, "unknown key %q: the keys are %s; attributes start with @ (@%s:…); quote text that contains a colon",
			key, strings.Join(ReservedKeys, ", "), key)
	}
	p.i += len(key) + 1
	v, err := p.value(key)
	if err != nil {
		return nil, err
	}
	return Label{Key: key, Value: v}, nil
}

// value reads a quoted or bare operand that must not be empty.
func (p *parser) value(what string) (string, error) {
	if p.eof() || isSpace(p.s[p.i]) || p.s[p.i] == ')' {
		return "", p.errHere("%s: needs a value after the colon", what)
	}
	if p.s[p.i] == '"' {
		return p.quoted()
	}
	return p.bare(), nil
}

// bare reads up to whitespace or a parenthesis.
func (p *parser) bare() string {
	start := p.i
	for p.i < len(p.s) && !isSpace(p.s[p.i]) && p.s[p.i] != '(' && p.s[p.i] != ')' {
		p.i++
	}
	return p.s[start:p.i]
}

func (p *parser) quoted() (string, error) {
	open := p.i
	p.i++
	var b strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch c {
		case '"':
			p.i++
			return b.String(), nil
		case '\\':
			if p.i+1 >= len(p.s) {
				return "", p.errAt(open, "unterminated string")
			}
			p.i++
			switch e := p.s[p.i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"', '\\':
				b.WriteByte(e)
			default:
				return "", p.errHere("unknown escape \\%c (use \\\", \\\\, \\n, \\t or \\r)", e)
			}
			p.i++
		default:
			b.WriteByte(c)
			p.i++
		}
	}
	return "", p.errAt(open, "unterminated string")
}

func (p *parser) attr() (Node, error) {
	at := p.i
	p.i++ // '@'
	start := p.i
	for p.i < len(p.s) {
		c := p.s[p.i]
		if c == '_' || c == '.' || c == '-' || ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') {
			p.i++
			continue
		}
		break
	}
	path := p.s[start:p.i]
	if path == "" {
		return nil, p.errAt(at, "'@' needs an attribute name, like @duration:>500")
	}
	if p.eof() || p.s[p.i] != ':' {
		return nil, p.errHere("@%s needs ':' and a value, like @%s:error", path, path)
	}
	p.i++
	if p.eof() || isSpace(p.s[p.i]) || p.s[p.i] == ')' {
		return nil, p.errHere("@%s: needs a value after the colon", path)
	}
	switch c := p.s[p.i]; c {
	case '>', '<':
		op := OpGt
		if c == '<' {
			op = OpLt
		}
		p.i++
		if !p.eof() && p.s[p.i] == '=' {
			p.i++
			op++ // OpGe, OpLe
		}
		numAt := p.i
		f, err := p.number(numAt)
		if err != nil {
			return nil, err
		}
		return Attr{Path: path, Op: op, Num: f}, nil
	case '[':
		return p.rangeTerm(path)
	}
	v, err := p.value("@" + path)
	if err != nil {
		return nil, err
	}
	return Attr{Path: path, Op: OpEq, Value: v}, nil
}

func (p *parser) number(at int) (float64, error) {
	word := p.bare()
	f, err := strconv.ParseFloat(word, 64)
	if word == "" || err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, p.errAt(at, "%q is not a number", word)
	}
	return f, nil
}

// rangeTerm parses `[lo TO hi]` after the cursor is on the '['.
func (p *parser) rangeTerm(path string) (Node, error) {
	open := p.i
	end := strings.IndexByte(p.s[p.i:], ']')
	if end < 0 {
		return nil, p.errAt(open, "range is missing its ']'")
	}
	inner := p.s[p.i+1 : p.i+end]
	parts := strings.Fields(inner)
	if len(parts) != 3 || parts[1] != "TO" {
		return nil, p.errAt(open, "a range reads [low TO high], like @ms:[100 TO 500]")
	}
	lo, err1 := strconv.ParseFloat(parts[0], 64)
	hi, err2 := strconv.ParseFloat(parts[2], 64)
	bad := func(f float64) bool { return math.IsNaN(f) || math.IsInf(f, 0) }
	if err1 != nil || err2 != nil || bad(lo) || bad(hi) {
		return nil, p.errAt(open, "range ends must be numbers: [%s TO %s]", parts[0], parts[2])
	}
	if lo > hi {
		return nil, p.errAt(open, "range [%s TO %s] is empty: the low end is above the high end", parts[0], parts[2])
	}
	p.i += end + 1
	return Attr{Path: path, Op: OpRange, Num: lo, Hi: hi}, nil
}

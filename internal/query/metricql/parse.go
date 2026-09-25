package metricql

import (
	"fmt"
	"strings"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Limits on the text, not on what it costs to run: a query that parses can
// still be refused by the planner for selecting too many series. These bound
// the parse itself, which runs before anything has checked who is asking.
const (
	// MaxLen bounds the query text in bytes. A dashboard widget's query is a
	// line or two; a kilobyte is already a generated monster.
	MaxLen = 8192
	// MaxDepth bounds nesting. Recursive descent uses the goroutine stack, so
	// without this a few kilobytes of '(' is a crash rather than an error.
	MaxDepth = 32
	// MaxValues bounds an IN list.
	MaxValues = 128
	// MaxSeconds bounds a modifier's second argument. A month of seconds is
	// far past any rollup a chart can draw.
	MaxSeconds = 31 * 24 * 60 * 60
)

// Parse turns query text into an AST.
//
// The error is always a [*Error], carrying the column to underline.
func Parse(q string) (Node, error) {
	if len(q) > MaxLen {
		return nil, errAt(MaxLen, "the query is %d bytes, over the %d-byte limit", len(q), MaxLen)
	}
	p := &parser{lex: lexer{src: q}}
	if err := p.advance(); err != nil {
		return nil, err
	}
	n, err := p.expr()
	if err != nil {
		return nil, err
	}
	if p.tok.kind != tokEOF {
		return nil, errAt(p.tok.pos, "unexpected %s after the end of the query", p.tok.describe())
	}
	return n, nil
}

// parser is a recursive-descent parser holding one token of lookahead.
//
// The lookahead is what makes the lexer's modes work: the parser never reads
// ahead past a point where the mode could change, because every advance names
// the mode the *next* token will be read in, and the parser always knows what
// it is about to read.
type parser struct {
	lex   lexer
	tok   token
	depth int
}

func (p *parser) advance() error {
	t, err := p.lex.next()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

func (p *parser) advanceKey() error {
	t, err := p.lex.nextKey()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

func (p *parser) advanceValue(stop string) { p.tok = p.lex.nextValue(stop) }

// expect fails unless the lookahead is k. It does not consume: the caller
// chooses the mode the next token is read in.
func (p *parser) expect(k tokenKind) error {
	if p.tok.kind != k {
		return errAt(p.tok.pos, "expected %s but found %s", k, p.tok.describe())
	}
	return nil
}

// enter counts one level of nesting, so that deeply nested input fails with a
// message instead of a stack overflow.
func (p *parser) enter() error {
	p.depth++
	if p.depth > MaxDepth {
		return errAt(p.tok.pos, "the query nests more than %d levels deep", MaxDepth)
	}
	return nil
}

func (p *parser) leave() { p.depth-- }

// expr = term { ("+" | "-") term }
func (p *parser) expr() (Node, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	left, err := p.term()
	if err != nil {
		return nil, err
	}
	for p.tok.kind == tokPlus || p.tok.kind == tokMinus {
		op := OpAdd
		if p.tok.kind == tokMinus {
			op = OpSub
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.term()
		if err != nil {
			return nil, err
		}
		left = &Binary{Op: op, Left: left, Right: right}
	}
	return left, nil
}

// term = factor { ("*" | "/") factor }
func (p *parser) term() (Node, error) {
	left, err := p.factor()
	if err != nil {
		return nil, err
	}
	for p.tok.kind == tokStar || p.tok.kind == tokSlash {
		op := OpMul
		if p.tok.kind == tokSlash {
			op = OpDiv
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
		right, err := p.factor()
		if err != nil {
			return nil, err
		}
		left = &Binary{Op: op, Left: left, Right: right}
	}
	return left, nil
}

// factor = "-" factor | number | query | func_call | "(" expr ")"
func (p *parser) factor() (Node, error) {
	if err := p.enter(); err != nil {
		return nil, err
	}
	defer p.leave()
	switch p.tok.kind {
	case tokMinus:
		if err := p.advance(); err != nil {
			return nil, err
		}
		x, err := p.factor()
		if err != nil {
			return nil, err
		}
		return &Unary{Op: OpSub, X: x}, nil
	case tokNumber:
		n := &Number{Value: p.tok.num}
		return n, p.advance()
	case tokLParen:
		if err := p.advance(); err != nil {
			return nil, err
		}
		inner, err := p.expr()
		if err != nil {
			return nil, err
		}
		if err := p.expect(tokRParen); err != nil {
			return nil, err
		}
		return inner, p.advance()
	case tokString:
		return nil, errAt(p.tok.pos, "a quoted string is only allowed as a function argument")
	case tokIdent:
		return p.identifier()
	}
	return nil, errAt(p.tok.pos, "expected a query, a number or '(' but found %s", p.tok.describe())
}

// identifier resolves the one place the grammar needs to look past a name: a
// query opens `agg:` and a call opens `name(`, and nothing else may start
// with a name.
func (p *parser) identifier() (Node, error) {
	name := p.tok
	if err := p.advance(); err != nil {
		return nil, err
	}
	switch p.tok.kind {
	case tokColon:
		return p.query(name)
	case tokLParen:
		return p.call(name)
	}
	return nil, errAt(name.pos,
		"%q is not a query or a function call: write an aggregator and a metric (sum:%s{*}) or a call (%s(…))",
		name.text, name.text, name.text)
}

// query = space_agg ":" metric "{" filter "}" [ "by" "{" keys "}" ] { "." modifier }
//
// The lookahead is the ':' when this is called.
func (p *parser) query(agg token) (Node, error) {
	a := Agg(strings.ToLower(agg.text))
	if _, ok := aggs[a]; !ok {
		return nil, errAt(agg.pos, "unknown aggregator %q (want avg, sum, min, max, count, or p50/p75/p90/p95/p99)", agg.text)
	}
	q := &Query{Agg: a}
	if err := p.advance(); err != nil {
		return nil, err
	}
	if p.tok.kind != tokIdent {
		return nil, errAt(p.tok.pos, "expected a metric name after %q but found %s", string(a)+":", p.tok.describe())
	}
	if !wire.ValidMetricName(p.tok.text) {
		return nil, errAt(p.tok.pos, "%q is not a valid metric name (letters, digits, '_' and '.', starting with a letter, up to %d bytes)", p.tok.text, wire.MaxMetricNameLen)
	}
	q.Metric = p.tok.text
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.expect(tokLBrace); err != nil {
		return nil, err
	}
	filter, err := p.filter()
	if err != nil {
		return nil, err
	}
	q.Filter = filter
	if err := p.advance(); err != nil { // past '}'
		return nil, err
	}
	// "by" and "IN" are the language's two keywords, and both are accepted in
	// any case, as aggregators are. A tag key is lower-cased when it is read
	// and a metric name is not, because that is how each is stored.
	if p.tok.kind == tokIdent && strings.EqualFold(p.tok.text, "by") {
		if q.By, err = p.by(); err != nil {
			return nil, err
		}
	}
	if q.Modifiers, err = p.modifiers(); err != nil {
		return nil, err
	}
	return q, nil
}

// filter = "*" | matcher { "," matcher }
//
// The lookahead is the '{' on entry and the '}' on return, so the caller
// decides which mode reads what follows.
func (p *parser) filter() (Filter, error) {
	if err := p.advanceKey(); err != nil {
		return nil, err
	}
	if p.tok.kind == tokStar {
		if err := p.advance(); err != nil {
			return nil, err
		}
		return nil, p.expect(tokRBrace)
	}
	if p.tok.kind == tokRBrace {
		return nil, errAt(p.tok.pos, "an empty filter: write {*} to select every series of the metric")
	}
	var out Filter
	for {
		m, err := p.matcher()
		if err != nil {
			return nil, err
		}
		out = append(out, m)
		if p.tok.kind != tokComma {
			break
		}
		if err := p.advanceKey(); err != nil {
			return nil, err
		}
	}
	if p.tok.kind != tokRBrace {
		return nil, errAt(p.tok.pos, "expected ',' or '}' but found %s", p.tok.describe())
	}
	return out, nil
}

// matcher = [ "!" ] key ":" value | [ "!" ] key " IN (" values ")" | "$" var
func (p *parser) matcher() (Matcher, error) {
	var m Matcher
	if p.tok.kind == tokBang {
		m.Neg = true
		if err := p.advanceKey(); err != nil {
			return m, err
		}
	}
	if p.tok.kind == tokVar {
		if m.Neg {
			return m, errAt(p.tok.pos-1, "a template variable cannot be negated: put the '!' in what $%s expands to", p.tok.text)
		}
		m.Var = p.tok.text
		return m, p.advance()
	}
	if p.tok.kind != tokKey {
		return m, errAt(p.tok.pos, "expected a tag key but found %s", p.tok.describe())
	}
	key := p.tok
	m.Key = key.text
	if !wire.ValidTag(m.Key) {
		return m, errAt(key.pos, "%q is not a valid tag key (lower-case letter, then letters, digits, '_', '.', '-' or '/', up to %d bytes)", key.text, wire.MaxTagKeyLen)
	}
	if err := p.advance(); err != nil {
		return m, err
	}
	switch {
	case p.tok.kind == tokColon:
		p.advanceValue(",}{")
		value := p.tok
		if value.text == "" {
			return m, errAt(value.pos, "expected a value after %q (write %s:* to match any value)", m.Key+":", m.Key)
		}
		if err := checkTag(m.Key, value); err != nil {
			return m, err
		}
		m.Values = []string{value.text}
		return m, p.advance()
	case p.tok.kind == tokIdent && strings.EqualFold(p.tok.text, "in"):
		m.In = true
		if err := p.advance(); err != nil {
			return m, err
		}
		if err := p.expect(tokLParen); err != nil {
			return m, err
		}
		for {
			p.advanceValue(",){")
			value := p.tok
			if value.text == "" {
				return m, errAt(value.pos, "expected a value in the list for %q", m.Key)
			}
			if err := checkTag(m.Key, value); err != nil {
				return m, err
			}
			if len(m.Values) == MaxValues {
				return m, errAt(value.pos, "more than %d values in one IN list", MaxValues)
			}
			m.Values = append(m.Values, value.text)
			if err := p.advance(); err != nil {
				return m, err
			}
			if p.tok.kind != tokComma {
				break
			}
		}
		if err := p.expect(tokRParen); err != nil {
			return m, err
		}
		return m, p.advance()
	}
	return m, errAt(p.tok.pos, "expected ':' or ' IN (' after the tag key %q but found %s", m.Key, p.tok.describe())
}

// checkTag holds a matcher to the same rules as a stored tag, so a filter
// that cannot possibly match is rejected where it is written rather than
// quietly returning nothing.
func checkTag(key string, value token) error {
	if !wire.ValidTag(key + ":" + value.text) {
		return errAt(value.pos, "%q is not a valid tag value: a tag is at most %d bytes and may not contain a comma or a newline", value.text, wire.MaxTagLen)
	}
	return nil
}

// by = "by" "{" key { "," key } "}"
//
// The lookahead is the "by" on entry, and whatever follows the '}' on return.
func (p *parser) by() ([]string, error) {
	if err := p.advance(); err != nil {
		return nil, err
	}
	if err := p.expect(tokLBrace); err != nil {
		return nil, err
	}
	var keys []string
	for {
		if err := p.advanceKey(); err != nil {
			return nil, err
		}
		if p.tok.kind != tokKey {
			return nil, errAt(p.tok.pos, "expected a tag key to group by but found %s", p.tok.describe())
		}
		if !wire.ValidTag(p.tok.text) {
			return nil, errAt(p.tok.pos, "%q is not a valid tag key", p.tok.text)
		}
		for _, k := range keys {
			if k == p.tok.text {
				return nil, errAt(p.tok.pos, "%q is already in the group-by", k)
			}
		}
		keys = append(keys, p.tok.text)
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.tok.kind != tokComma {
			break
		}
	}
	if p.tok.kind != tokRBrace {
		return nil, errAt(p.tok.pos, "expected ',' or '}' but found %s", p.tok.describe())
	}
	return keys, p.advance()
}

// modifiers = { "." modifier }
func (p *parser) modifiers() ([]Modifier, error) {
	var out []Modifier
	for p.tok.kind == tokDot {
		if err := p.advance(); err != nil {
			return nil, err
		}
		if p.tok.kind != tokIdent {
			return nil, errAt(p.tok.pos, "expected a modifier name after '.' but found %s", p.tok.describe())
		}
		name := p.tok
		m, err := p.modifier(name)
		if err != nil {
			return nil, err
		}
		for _, prev := range out {
			if prev.Kind == m.Kind {
				return nil, errAt(name.pos, "%s is already set on this query", m.Kind)
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// modifier is one `.name(args)`; the lookahead is the name on entry.
func (p *parser) modifier(name token) (Modifier, error) {
	m := Modifier{Kind: ModKind(name.text)}
	if err := p.advance(); err != nil {
		return m, err
	}
	if err := p.expect(tokLParen); err != nil {
		return m, err
	}
	if err := p.advance(); err != nil {
		return m, err
	}
	switch m.Kind {
	case ModAsRate, ModAsCount:
		if p.tok.kind != tokRParen {
			return m, errAt(p.tok.pos, "%s takes no arguments", m.Kind)
		}
	case ModRollup, ModFill:
		allowed := rollupMethods
		what := "rollup method"
		if m.Kind == ModFill {
			allowed, what = fillModes, "fill mode"
		}
		if p.tok.kind != tokIdent || !oneOf(p.tok.text, allowed) {
			return m, errAt(p.tok.pos, "expected a %s (%s) but found %s", what, quoteList(allowed), p.tok.describe())
		}
		m.Method = p.tok.text
		if err := p.advance(); err != nil {
			return m, err
		}
		if p.tok.kind == tokComma {
			if err := p.advance(); err != nil {
				return m, err
			}
			secs, err := p.seconds()
			if err != nil {
				return m, err
			}
			m.Seconds = secs
		}
	default:
		return m, errAt(name.pos, "unknown modifier %q (want rollup, as_rate, as_count or fill)", name.text)
	}
	if err := p.expect(tokRParen); err != nil {
		return m, err
	}
	return m, p.advance()
}

// seconds reads a modifier's duration argument: a whole number of seconds,
// at least one. Zero is rejected rather than treated as absent, so that a
// [Modifier] with Seconds == 0 unambiguously means "not written".
func (p *parser) seconds() (int64, error) {
	if p.tok.kind != tokNumber {
		return 0, errAt(p.tok.pos, "expected a number of seconds but found %s", p.tok.describe())
	}
	v := p.tok.num
	secs := int64(v)
	if float64(secs) != v || secs < 1 || secs > MaxSeconds {
		return 0, errAt(p.tok.pos, "%s is not a whole number of seconds from 1 to %d", p.tok.text, MaxSeconds)
	}
	return secs, p.advance()
}

// call = ident "(" [ arg { "," arg } ] ")"
//
// The lookahead is the '(' when this is called.
func (p *parser) call(name token) (Node, error) {
	sig, ok := Functions[name.text]
	if !ok {
		return nil, errAt(name.pos, "unknown function %q (have %s)", name.text, quoteList(FunctionNames()))
	}
	if err := p.advance(); err != nil {
		return nil, err
	}
	c := &Call{Func: name.text}
	if p.tok.kind != tokRParen {
		for {
			if len(c.Args) == len(sig.Args) {
				return nil, errAt(p.tok.pos, "%s takes at most %s", name.text, plural(len(sig.Args), "argument"))
			}
			arg, err := p.argument(name.text, sig.Args[len(c.Args)], len(c.Args))
			if err != nil {
				return nil, err
			}
			c.Args = append(c.Args, arg)
			if p.tok.kind != tokComma {
				break
			}
			if err := p.advance(); err != nil {
				return nil, err
			}
		}
	}
	if err := p.expect(tokRParen); err != nil {
		return nil, err
	}
	if len(c.Args) < sig.Required {
		return nil, errAt(name.pos, "%s takes %s but was given %d", name.text, plural(sig.Required, "argument"), len(c.Args))
	}
	return c, p.advance()
}

// argument reads one argument and holds it to the position's kind. The kinds
// are checked here rather than after the parse so that the error can point at
// the argument instead of at the call.
func (p *parser) argument(fn string, kind ArgKind, index int) (Node, error) {
	if set := kind.values(); set != nil {
		if p.tok.kind != tokString || !oneOf(p.tok.text, set) {
			return nil, errAt(p.tok.pos, "argument %d of %s must be one of %s", index+1, fn, quoteList(set))
		}
		s := &String{Value: p.tok.text}
		return s, p.advance()
	}
	at := p.tok
	n, err := p.expr()
	if err != nil {
		return nil, err
	}
	if kind == ArgNumber {
		if _, ok := literalNumber(n); !ok {
			return nil, errAt(at.pos, "argument %d of %s must be a plain number, not an expression", index+1, fn)
		}
	}
	return n, nil
}

// literalNumber unwraps a number written in the source, negated or not.
func literalNumber(n Node) (float64, bool) {
	switch v := n.(type) {
	case *Number:
		return v.Value, true
	case *Unary:
		if inner, ok := v.X.(*Number); ok {
			return -inner.Value, true
		}
	}
	return 0, false
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

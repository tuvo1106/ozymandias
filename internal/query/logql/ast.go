package logql

import (
	"strconv"
	"strings"
)

// Node is a node of a parsed query. The concrete types are [Or], [And],
// [Not], [Text], [Label] and [Attr]; the unexported method keeps the set
// closed, so a switch over them is exhaustive by construction.
type Node interface {
	// String prints the node as canonical query text: parsing it gives back
	// an equal tree, which is what lets the API echo a normalized query and
	// a saved view be compared as text.
	String() string
	node()
}

// Or is a disjunction of two or more terms.
type Or struct{ Args []Node }

// And is a conjunction of two or more terms. Juxtaposition (`a b`) and `AND`
// both build it; a parenthesized And inside an And stays a separate node, so
// the tree records what was written and printing it adds the same parens.
type And struct{ Args []Node }

// Not negates its operand: `-x` and `NOT x` both build it.
type Not struct{ X Node }

// Text is a free-text term: a case-insensitive substring of the message or
// of any string attribute value, with `*` as a wildcard for any run of
// characters. "*" alone matches every log.
type Text struct{ Value string }

// Label is a `key:value` term on one of the reserved keys ([ReservedKeys]):
// the fields every log has. Value may contain `*` wildcards and is matched
// against the whole field, case-sensitively.
type Label struct{ Key, Value string }

// Op is how an [Attr] term compares an attribute with its operand.
type Op int

const (
	// OpEq matches the attribute's value against Value (wildcards allowed;
	// "*" alone means "the attribute exists"). Numbers compare as numbers.
	OpEq Op = iota
	// OpGt compares a numeric attribute with Num: greater than.
	OpGt
	// OpGe is greater than or equal to Num.
	OpGe
	// OpLt is less than Num.
	OpLt
	// OpLe is less than or equal to Num.
	OpLe
	// OpRange is `[Num TO Hi]`, both ends inclusive.
	OpRange
)

// Attr is an `@path:…` term on an attribute of the structured log, where
// path is dotted (`@http.status`): nesting is flattened here, at query time,
// not at ingest (docs/wire-protocol.md §E).
type Attr struct {
	Path  string
	Op    Op
	Value string  // OpEq
	Num   float64 // OpGt, OpGe, OpLt, OpLe; the low end of OpRange
	Hi    float64 // the high end of OpRange
}

func (Or) node()    {}
func (And) node()   {}
func (Not) node()   {}
func (Text) node()  {}
func (Label) node() {}
func (Attr) node()  {}

// ReservedKeys are the keys a `key:value` term may use, in the order the UI
// lists them. Everything else is an attribute and needs the `@` prefix: a
// bare `error:timeout` is far more likely a mistake than a free-text search,
// and the parser says so instead of guessing.
var ReservedKeys = []string{"service", "source", "host", "status", "env", "trace_id"}

func isReserved(k string) bool {
	for _, r := range ReservedKeys {
		if r == k {
			return true
		}
	}
	return false
}

func (n Or) String() string {
	parts := make([]string, len(n.Args))
	for i, a := range n.Args {
		parts[i] = a.String()
		if _, ok := a.(Or); ok {
			parts[i] = "(" + parts[i] + ")"
		}
	}
	return strings.Join(parts, " OR ")
}

func (n And) String() string {
	parts := make([]string, len(n.Args))
	for i, a := range n.Args {
		parts[i] = a.String()
		switch a.(type) {
		case Or, And:
			parts[i] = "(" + parts[i] + ")"
		}
	}
	return strings.Join(parts, " ")
}

func (n Not) String() string {
	s := n.X.String()
	switch n.X.(type) {
	case Or, And:
		s = "(" + s + ")"
	}
	return "-" + s
}

func (n Text) String() string {
	if bareSafe(n.Value) && n.Value[0] != '-' && n.Value[0] != '@' &&
		!strings.Contains(n.Value, ":") && !isKeyword(n.Value) {
		return n.Value
	}
	return quote(n.Value)
}

func (n Label) String() string { return n.Key + ":" + valueText(n.Value) }

func (n Attr) String() string {
	p := "@" + n.Path + ":"
	switch n.Op {
	case OpGt:
		return p + ">" + num(n.Num)
	case OpGe:
		return p + ">=" + num(n.Num)
	case OpLt:
		return p + "<" + num(n.Num)
	case OpLe:
		return p + "<=" + num(n.Num)
	case OpRange:
		return p + "[" + num(n.Num) + " TO " + num(n.Hi) + "]"
	}
	if v := n.Value; v != "" && v[0] != '>' && v[0] != '<' && v[0] != '[' {
		return p + valueText(v)
	}
	return p + quote(n.Value)
}

func valueText(v string) string {
	if bareSafe(v) {
		return v
	}
	return quote(v)
}

func num(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

func isKeyword(s string) bool { return s == "AND" || s == "OR" || s == "NOT" }

// bareSafe reports whether s can be written without quotes: non-empty, with
// nothing the parser would stop at or treat specially.
func bareSafe(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c <= ' ' || c == 0x7f, c == '(', c == ')', c == '"', c == '\\':
			return false
		}
	}
	return true
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

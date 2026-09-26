package metricql

import (
	"fmt"
	"strconv"
	"strings"
)

// tokenKind is a token's lexical class.
type tokenKind int

const (
	tokEOF tokenKind = iota
	tokIdent
	tokNumber
	tokString
	tokVar
	tokKey
	tokValue
	tokLParen
	tokRParen
	tokLBrace
	tokRBrace
	tokComma
	tokColon
	tokDot
	tokBang
	tokPlus
	tokMinus
	tokStar
	tokSlash
)

// kindNames name a token class the way "expected …" reads best.
var kindNames = map[tokenKind]string{
	tokEOF:    "the end of the query",
	tokIdent:  "a name",
	tokNumber: "a number",
	tokString: "a quoted string",
	tokVar:    "a $variable",
	tokKey:    "a tag key",
	tokValue:  "a tag value",
	tokLParen: "'('",
	tokRParen: "')'",
	tokLBrace: "'{'",
	tokRBrace: "'}'",
	tokComma:  "','",
	tokColon:  "':'",
	tokDot:    "'.'",
	tokBang:   "'!'",
	tokPlus:   "'+'",
	tokMinus:  "'-'",
	tokStar:   "'*'",
	tokSlash:  "'/'",
}

func (k tokenKind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	return "an unknown token"
}

var punctuation = map[byte]tokenKind{
	'(': tokLParen, ')': tokRParen, '{': tokLBrace, '}': tokRBrace,
	',': tokComma, ':': tokColon, '.': tokDot, '!': tokBang,
	'+': tokPlus, '-': tokMinus, '*': tokStar, '/': tokSlash,
}

// token is one lexeme. text is the source text with the parts that were only
// punctuation removed: a string's quotes and escapes, a variable's '$'.
type token struct {
	kind tokenKind
	text string
	pos  int
	// num is the parsed value of a tokNumber.
	num float64
}

// describe names the token as it appears in the query, for an error that has
// to say what it found rather than what it wanted.
func (t token) describe() string {
	switch t.kind {
	case tokEOF:
		return "the end of the query"
	case tokIdent, tokKey, tokValue:
		return strconv.Quote(t.text)
	case tokNumber:
		return "the number " + t.text
	case tokString:
		return "the string " + strconv.Quote(t.text)
	case tokVar:
		return "$" + t.text
	default:
		return "'" + t.text + "'"
	}
}

// lexer is a cursor over the query text. Its entire state is the source and
// an offset, which is what lets the parser ask for the token at a position in
// a different mode after the fact — see the package comment.
type lexer struct {
	src string
	pos int
}

// whitespace is what separates tokens, and — because [lexer.nextValue] trims
// it — what a tag value may not begin or end with. One definition, so the two
// cannot drift apart.
const whitespace = " \t\n\r"

func (l *lexer) skipSpace() {
	for l.pos < len(l.src) && strings.ContainsRune(whitespace, rune(l.src[l.pos])) {
		l.pos++
	}
}

// next scans the next token in expression mode: names, numbers, strings,
// variables and operators. This is the mode everywhere outside a `{…}`.
func (l *lexer) next() (token, error) {
	l.skipSpace()
	start := l.pos
	if l.pos >= len(l.src) {
		return token{kind: tokEOF, pos: start}, nil
	}
	c := l.src[l.pos]
	switch {
	case isLetter(c):
		for l.pos < len(l.src) && isNameChar(l.src[l.pos]) {
			l.pos++
		}
		return token{kind: tokIdent, text: l.src[start:l.pos], pos: start}, nil
	case isDigit(c):
		return l.number(start)
	case c == '$':
		return l.variable(start)
	case c == '"':
		return l.quoted(start)
	}
	if k, ok := punctuation[c]; ok {
		l.pos++
		return token{kind: k, text: string(c), pos: start}, nil
	}
	return token{}, errAt(start, "unexpected character %s", strconv.QuoteRune(rune(c)))
}

// nextKey scans a tag key, and anything that is not one the ordinary way.
//
// Key mode exists because a tag key may contain '-' and '/', which are
// operators everywhere else. Falling through to next for a non-key byte is
// what lets one mode serve the whole inside of a filter: '!', '*', '$name',
// '}' and ',' all still come out as themselves.
func (l *lexer) nextKey() (token, error) {
	l.skipSpace()
	start := l.pos
	if l.pos >= len(l.src) || !isLetter(l.src[l.pos]) {
		return l.next()
	}
	for l.pos < len(l.src) && isKeyChar(l.src[l.pos]) {
		l.pos++
	}
	// Lower-cased here because the intake lower-cases every tag it stores
	// (pkg/wire.NormalizeTag), so a key typed in capitals would otherwise
	// select nothing and give the user no clue why.
	return token{kind: tokKey, text: strings.ToLower(l.src[start:l.pos]), pos: start}, nil
}

// nextValue scans a tag value: every byte up to the first one in stop.
//
// A value is free text — `route:/api/items`, `url:http://x`, `version:1.2-rc*`
// — so it has no lexical structure to speak of and is delimited only by what
// follows it. Surrounding whitespace is trimmed, so `a, b` inside an IN list is
// the two values a and b.
//
// "Whitespace" here has to mean exactly what it means between tokens, newlines
// and carriage returns included, or a filter written across two lines — which
// the grammar allows and a long query invites — ends its value with a newline
// and is rejected for containing one. A stray '\r' is worse: no rule forbids
// it inside a tag, so the matcher would be accepted, printed back faithfully,
// and never match anything.
//
// A value therefore cannot contain ',', '{', '}' or, in a list, ')'. The
// comma is the wire format's own tag separator so no stored tag has one
// either; the braces are given up deliberately. Without them, forgetting the
// closing brace — `{service:api by {route}` — makes " by {route" part of the
// value and the whole query parses, wrong and silent. Stopping at a brace
// turns the commonest typo in the language into an error at the right place,
// and a tag value containing one is not a thing anybody writes.
// docs/query-language.md records this as the limit it is.
func (l *lexer) nextValue(stop string) token {
	l.skipSpace()
	start := l.pos
	for l.pos < len(l.src) && !strings.ContainsRune(stop, rune(l.src[l.pos])) {
		l.pos++
	}
	text := strings.TrimRight(l.src[start:l.pos], whitespace)
	return token{kind: tokValue, text: text, pos: start}
}

// number scans an unsigned decimal, with an optional fraction and exponent.
// A '.' is only part of the number when a digit follows it, so `3.` is the
// number 3 followed by a modifier dot that will fail to parse — a better
// error than "malformed number".
func (l *lexer) number(start int) (token, error) {
	for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
		l.pos++
	}
	if l.pos+1 < len(l.src) && l.src[l.pos] == '.' && isDigit(l.src[l.pos+1]) {
		l.pos++
		for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			l.pos++
		}
	}
	if l.pos < len(l.src) && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
		mark := l.pos
		l.pos++
		if l.pos < len(l.src) && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
			l.pos++
		}
		if l.pos < len(l.src) && isDigit(l.src[l.pos]) {
			for l.pos < len(l.src) && isDigit(l.src[l.pos]) {
				l.pos++
			}
		} else {
			l.pos = mark // "1e" is the number 1 followed by the name e
		}
	}
	text := l.src[start:l.pos]
	v, err := strconv.ParseFloat(text, 64)
	if err != nil {
		// Only out of range gets here: the scan above accepts exactly what
		// ParseFloat accepts otherwise.
		return token{}, errAt(start, "%s is too large to be a number", text)
	}
	return token{kind: tokNumber, text: text, pos: start, num: v}, nil
}

// variable scans `$name`. Variable names are plain identifiers, not tag keys:
// a '-' or '/' in one would be unreadable next to arithmetic, and the name is
// chosen by whoever writes the dashboard.
func (l *lexer) variable(start int) (token, error) {
	l.pos++ // '$'
	from := l.pos
	for l.pos < len(l.src) && isNameChar(l.src[l.pos]) {
		l.pos++
	}
	if l.pos == from {
		return token{}, errAt(start, "expected a variable name after '$'")
	}
	return token{kind: tokVar, text: l.src[from:l.pos], pos: start}, nil
}

// quoted scans a double-quoted string with \\ and \" escapes. It is the one
// place the language has quoting at all, for the string arguments of
// functions like top(…, "mean", "desc").
func (l *lexer) quoted(start int) (token, error) {
	l.pos++ // '"'
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch c {
		case '"':
			l.pos++
			return token{kind: tokString, text: b.String(), pos: start}, nil
		case '\\':
			if l.pos+1 >= len(l.src) {
				return token{}, errAt(l.pos, "the query ends inside an escape")
			}
			esc := l.src[l.pos+1]
			if esc != '"' && esc != '\\' {
				return token{}, errAt(l.pos, "unknown escape %s (only \\\" and \\\\ are understood)", strconv.Quote(`\`+string(esc)))
			}
			b.WriteByte(esc)
			l.pos += 2
		case '\n':
			return token{}, errAt(start, "a string cannot span lines")
		default:
			b.WriteByte(c)
			l.pos++
		}
	}
	return token{}, errAt(start, "unterminated string")
}

func isLetter(c byte) bool { return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') }
func isDigit(c byte) bool  { return '0' <= c && c <= '9' }

// isNameChar is the metric-name and identifier alphabet, matching
// pkg/wire.ValidMetricName.
func isNameChar(c byte) bool { return isLetter(c) || isDigit(c) || c == '_' || c == '.' }

// isKeyChar is the tag-key alphabet, matching pkg/wire's, plus upper case
// because nextKey lower-cases what it reads.
func isKeyChar(c byte) bool {
	return isNameChar(c) || c == '-' || c == '/'
}

// Error is a parse failure, located.
type Error struct {
	// Msg says what went wrong, without the position.
	Msg string
	// Col is the 1-based byte offset the failure points at, which is what the
	// query editor underlines.
	Col int
}

func (e *Error) Error() string { return fmt.Sprintf("col %d: %s", e.Col, e.Msg) }

func errAt(pos int, format string, a ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, a...), Col: pos + 1}
}

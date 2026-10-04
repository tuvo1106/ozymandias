package logql

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// Filter reports whether a log satisfies a query. The store's scan and the
// live-tail hub both run it, so a query means the same thing on stored logs
// and on logs still arriving.
type Filter func(*wire.Log) bool

// Compile turns a parsed query into a [Filter]. A nil Node (the empty
// query) matches every log. Patterns are prepared once here, because the
// filter runs once per log, over gigabytes.
func Compile(n Node) Filter {
	switch n := n.(type) {
	case nil:
		return func(*wire.Log) bool { return true }
	case Or:
		fs := compileAll(n.Args)
		return func(l *wire.Log) bool {
			for _, f := range fs {
				if f(l) {
					return true
				}
			}
			return false
		}
	case And:
		fs := compileAll(n.Args)
		return func(l *wire.Log) bool {
			for _, f := range fs {
				if !f(l) {
					return false
				}
			}
			return true
		}
	case Not:
		f := Compile(n.X)
		return func(l *wire.Log) bool { return !f(l) }
	case Text:
		return compileText(n)
	case Label:
		return compileLabel(n)
	case Attr:
		return compileAttr(n)
	}
	panic("logql: unknown node type") // Node is closed; unreachable
}

func compileAll(ns []Node) []Filter {
	fs := make([]Filter, len(ns))
	for i, n := range ns {
		fs[i] = Compile(n)
	}
	return fs
}

// compileText matches the message, then every string attribute value.
func compileText(t Text) Filter {
	// A pattern with no literal part ("*", "**", "") has no segments, and
	// containsWild with none is true: it matches every log.
	segs := splitWild(strings.ToLower(t.Value))
	return func(l *wire.Log) bool {
		if containsWild(l.Message, segs) {
			return true
		}
		return anyString(l.Attrs, func(s string) bool { return containsWild(s, segs) })
	}
}

func anyString(v any, fn func(string) bool) bool {
	switch v := v.(type) {
	case string:
		return fn(v)
	case map[string]any:
		for _, e := range v {
			if anyString(e, fn) {
				return true
			}
		}
	case []any:
		for _, e := range v {
			if anyString(e, fn) {
				return true
			}
		}
	}
	return false
}

func compileLabel(lb Label) Filter {
	key, pat := lb.Key, lb.Value
	return func(l *wire.Log) bool { return MatchLabel(pat, LabelValue(l, key)) }
}

// LabelValue is a log's value for a reserved key; "" when it has none (no env
// tag, no host). The store keys its streams on the same function, so the
// index and the scan cannot disagree about what a log's labels are.
func LabelValue(l *wire.Log, key string) string {
	switch key {
	case "service":
		return l.Service
	case "source":
		return l.Source
	case "host":
		return l.Host
	case "status":
		return l.Status
	case "trace_id":
		return l.TraceID
	case "env":
		// Tags are canonical (sorted), so "the first env tag" is stable.
		for _, t := range l.Tags {
			if k, v := wire.SplitTag(t); k == "env" {
				return v
			}
		}
	}
	return ""
}

// MatchLabel is how a Label term's value matches a label's value: `*` stands
// for any run of characters, and `*` by itself means "has a value", so
// `host:*` finds logs that name a host and `-host:*` those that do not.
func MatchLabel(pattern, value string) bool {
	if pattern == "*" {
		return value != ""
	}
	return globMatch(pattern, value)
}

func compileAttr(a Attr) Filter {
	return func(l *wire.Log) bool {
		for _, v := range lookup(l.Attrs, a.Path) {
			if attrMatches(a, v) {
				return true
			}
		}
		return false
	}
}

// lookup returns every value at a dotted path. Nesting is flattened here, at
// query time (wire-protocol.md §E), so a key may itself contain dots
// ("http.status" as one key) or be a path through objects, and arrays
// contribute each element. Trying a whole key before splitting is what lets
// both spellings of the same attribute be found.
func lookup(m map[string]any, path string) []any {
	var out []any
	var walk func(cur any, path string)
	walk = func(cur any, path string) {
		switch cur := cur.(type) {
		case map[string]any:
			if v, ok := cur[path]; ok {
				out = appendFlat(out, v)
			}
			for i := 0; i < len(path); i++ {
				if path[i] != '.' {
					continue
				}
				if sub, ok := cur[path[:i]]; ok {
					walk(sub, path[i+1:])
				}
			}
		case []any:
			for _, e := range cur {
				walk(e, path)
			}
		}
	}
	walk(m, path)
	return out
}

// AttrValues returns every value of the attribute at a dotted path, with the
// same flattening as an `@path:` term: it is how the log store counts the
// values of an attribute for a facet, so a facet and a filter agree on what
// "the attribute" is.
func AttrValues(attrs map[string]any, path string) []any { return lookup(attrs, path) }

// ValueText is how an attribute value reads as text: the digits a number was
// logged with, "true"/"false", "null".
func ValueText(v any) string {
	switch v := v.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case nil:
		return "null"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func appendFlat(out []any, v any) []any {
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			out = appendFlat(out, e)
		}
		return out
	}
	return append(out, v)
}

func attrMatches(a Attr, v any) bool {
	if a.Op == OpEq {
		return eqMatches(a.Value, v)
	}
	f, ok := numberOf(v)
	if !ok {
		return false
	}
	switch a.Op {
	case OpGt:
		return f > a.Num
	case OpGe:
		return f >= a.Num
	case OpLt:
		return f < a.Num
	case OpLe:
		return f <= a.Num
	case OpRange:
		return f >= a.Num && f <= a.Hi
	}
	return false
}

// eqMatches compares an attribute value with a pattern. Two numbers compare
// as numbers, so @status:200 finds 200 and 200.0 alike; everything else
// compares as the text it was logged as, with `*` as a wildcard.
func eqMatches(pat string, v any) bool {
	if pf, err := strconv.ParseFloat(pat, 64); err == nil {
		if f, ok := numberOf(v); ok {
			return f == pf
		}
	}
	switch v := v.(type) {
	case string:
		return globMatch(pat, v)
	case json.Number:
		return globMatch(pat, v.String())
	case float64:
		return globMatch(pat, strconv.FormatFloat(v, 'g', -1, 64))
	case bool:
		return globMatch(pat, strconv.FormatBool(v))
	case nil:
		return globMatch(pat, "null")
	}
	return false
}

func numberOf(v any) (float64, bool) {
	switch v := v.(type) {
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

// globMatch matches s against pat as a whole, `*` standing for any run
// (including none) of characters.
func globMatch(pat, s string) bool {
	if !strings.Contains(pat, "*") {
		return pat == s
	}
	segs := strings.Split(pat, "*")
	if !strings.HasPrefix(s, segs[0]) {
		return false
	}
	s = s[len(segs[0]):]
	last := segs[len(segs)-1]
	for _, seg := range segs[1 : len(segs)-1] {
		i := strings.Index(s, seg)
		if i < 0 {
			return false
		}
		s = s[i+len(seg):]
	}
	return strings.HasSuffix(s, last)
}

// splitWild splits a lowercased pattern on `*`, dropping empty segments:
// "a**b" and "a*b" are the same pattern, and a leading or trailing `*` adds
// nothing to a substring search.
func splitWild(pat string) []string {
	var segs []string
	for _, s := range strings.Split(pat, "*") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

// containsWild reports whether s contains the segments in order, compared
// without regard to case. segs are already lowercase.
//
// ASCII input (nearly every log line) is searched in place, with no
// allocation; anything else is lowercased once and searched as a string, so
// "É" still finds "é". The offsets only ever index the string being searched.
func containsWild(s string, segs []string) bool {
	if !isASCII(s) {
		s = strings.ToLower(s)
		from := 0
		for _, seg := range segs {
			i := strings.Index(s[from:], seg)
			if i < 0 {
				return false
			}
			from += i + len(seg)
		}
		return true
	}
	from := 0
	for _, seg := range segs {
		i := indexFoldASCII(s, seg, from)
		if i < 0 {
			return false
		}
		from = i + len(seg)
	}
	return true
}

// indexFoldASCII finds the lowercase needle in the ASCII string s at or
// after from, ignoring the case of s. A needle byte outside ASCII never
// equals a (lowered) ASCII byte, so a non-ASCII needle simply finds nothing.
func indexFoldASCII(s, needle string, from int) int {
	last := len(s) - len(needle)
	for i := from; i <= last; i++ {
		j := 0
		for j < len(needle) && lower(s[i+j]) == needle[j] {
			j++
		}
		if j == len(needle) {
			return i
		}
	}
	return -1
}

func lower(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

package logpipeline

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// Redacted is what a secret is replaced with.
const Redacted = "[REDACTED]"

// secretWords are the words that mark a field as holding something that must
// never reach the log store, as they appear in a key (`password`,
// `access_token`, `customer_phone`). They are deliberately few and are matched
// as whole words between separators, so `microphone` and `secretary` are not
// secrets and `token_count` is not a token. The two words that name a kind of
// secret only at the end of a key (`token`, `access_key`) are matched after any
// prefix, so `csrftoken` and `authtoken` are caught: a false positive hides a
// harmless value, a false negative leaks a secret, and the costs are not equal.
// Anything an app calls something else is a per-app rule
// (deploy/agent.d/<app>.yaml), not a default.
const (
	wordsAnywhere = `password|passwd|secret|api[_-]?key|private[_-]?key|credential|cookie|authorization|phone|ssn|card[_-]?number|cvv|cvc`
	wordsAtEnd    = `token|access[_-]?key`
)

// Patterns are in the order they are applied. Each keeps what is not secret
// (the key, the separator, the URL up to the value) and replaces only the
// value, so a redacted line still reads as the line it was.
var defaultTextRules = []textRule{
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`), Redacted, needNone},
	{"authorization", regexp.MustCompile(`(?i)\b((?:proxy-)?authorization)(["']?\s*[:=]\s*["']?)(?:(?:bearer|basic|token|digest)\s+)?[^\s"',;]+`), "${1}${2}" + Redacted, needNone},
	{"url-secret", regexp.MustCompile(`(?i)([?&](?:token|code|key|signature|sig|api_?key|access_token|id_token|refresh_token|password|secret|reset_token|verify_token)=)[^&\s"'#]+`), "${1}" + Redacted, needNone},
	// key=value, key: value, "key": "value", key=>"value" (Ruby's inspect). The value is
	// a quoted string (spaces allowed) or a bare run; a quoted one keeps its quotes.
	{"kv-secret-dq", kvRule(`"[^"]*"`), `${1}${2}"` + Redacted + `"`, needNone},
	{"kv-secret-sq", kvRule(`'[^']*'`), `${1}${2}'` + Redacted + `'`, needNone},
	// A bare value may not start with '>' (that is the tail of Ruby's =>) and an
	// already-redacted value is matched whole, so applying the rules twice
	// changes nothing: "[REDACTED]" would otherwise match as "[REDACTED" and
	// leave a stray bracket behind.
	{"kv-secret", kvRule(`\[REDACTED\]|[^\s"',;&})\]>][^\s"',;&})\]]*`), "${1}${2}" + Redacted, needNone},
}

var emailRule = textRule{"email", regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}\b`), Redacted, needNone}

// needKind names a default rule's cheap necessary condition (see textRule.need).
// An enum and a switch rather than closures: a call through a func value makes
// its argument escape, and the lowercased line is built in a stack buffer so that
// the common line costs no allocation before a filter can skip a rule.
type needKind uint8

const (
	needNone needKind = iota // always run (custom rules)
	needJWT
	needAuthorization
	needURLSecret
	needKV
	needEmail
)

// prefilter gives each default rule its necessary condition, keyed by rule name.
// `low` is the line lowercased (see lowerInto).
//
// Soundness is the whole point: a condition that wrongly says no lets a secret
// through unredacted. Each is a literal the rule's regular expression cannot
// match without, and TestRedactPrefilter_* checks them against the unfiltered
// rules over generated strings, and against the word lists.
func prefilter(name string) needKind {
	switch name {
	case "jwt":
		return needJWT
	case "authorization":
		return needAuthorization
	case "url-secret":
		return needURLSecret
	case "kv-secret-dq", "kv-secret-sq", "kv-secret":
		return needKV
	case "email":
		return needEmail
	}
	return needNone
}

// ok reports whether the rule might match the lowercased line low.
func (k needKind) ok(low []byte) bool {
	switch k {
	case needJWT:
		return bytes.Contains(low, []byte("eyj"))
	case needAuthorization:
		return bytes.Contains(low, []byte("authorization"))
	case needURLSecret:
		// [?&]word=
		return bytes.IndexByte(low, '=') >= 0 && (bytes.IndexByte(low, '?') >= 0 || bytes.IndexByte(low, '&') >= 0)
	case needKV:
		// A separator and a secret word, each somewhere in the line. Two independent
		// tests, not "the word near the separator": cheap, and sound because the
		// rule needs both.
		if bytes.IndexByte(low, '=') < 0 && bytes.IndexByte(low, ':') < 0 {
			return false
		}
		for _, w := range secretNeedles {
			if bytes.Contains(low, w) {
				return true
			}
		}
		return false
	case needEmail:
		return bytes.IndexByte(low, '@') >= 0
	}
	return true
}

// secretNeedles are substrings at least one of which every key the kv rules
// match contains: wordsAnywhere and wordsAtEnd, cut down to the part that does
// not vary (`api[_-]?key` always contains "key", `card[_-]?number` "number").
var secretNeedles = [][]byte{[]byte("passw"), []byte("secret"), []byte("key"), []byte("credential"), []byte("cookie"), []byte("authorization"), []byte("phone"), []byte("ssn"), []byte("number"), []byte("cvv"), []byte("cvc"), []byte("token")}

func kvRule(value string) *regexp.Regexp {
	key := `\b((?:[\w.-]*[._-])?(?:` + wordsAnywhere + `)(?:s|[._-][\w.-]*)?|[\w.-]*(?:` + wordsAtEnd + `)s?)`
	return regexp.MustCompile(`(?i)` + key + `(["']?\s*(?:=>|[:=])\s*)(?:` + value + `)`)
}

type textRule struct {
	name    string
	re      *regexp.Regexp
	replace string
	// need is a cheap necessary condition for re to match: it may say yes when
	// there is no match, never no when there is one. It is how almost every line
	// skips the regular expressions, which are most of the agent's per-line cost.
	// needNone means always run (custom rules). See [prefilter].
	need needKind
}

// RedactConfig adds to, or switches off parts of, the default rules.
type RedactConfig struct {
	// Disable names default rules to turn off: jwt, authorization,
	// url-secret, kv-secret, email.
	Disable []string `yaml:"disable"`
	// Rules are extra text patterns, applied after the defaults.
	Rules []RuleSpec `yaml:"rules"`
	// Keys are extra attribute key names (matched whole, case-insensitively,
	// after camelCase is read as snake_case) whose values are always redacted.
	Keys []string `yaml:"keys"`
}

// RuleSpec is one extra text rule: every match of Pattern is replaced with
// Replace, in which ${1} is the first group. An empty Replace means Redacted.
type RuleSpec struct {
	Pattern string `yaml:"pattern"`
	Replace string `yaml:"replace"`
}

// Redactor removes secrets from a log's message and attributes. It exists
// because a log line is the easiest place for a secret to leak: an app prints
// a request, a reset link, a header, and the line is copied, indexed, and kept
// for a week. The defaults are generic; they know nothing about any app.
type Redactor struct {
	text    []textRule
	keyRe   *regexp.Regexp
	extra   map[string]bool
	onMatch func(rule string)
}

// NewRedactor builds a redactor from the defaults and cfg. onMatch, if set,
// is called with a rule's name each time it changes a string (for counting).
func NewRedactor(cfg RedactConfig, onMatch func(rule string)) (*Redactor, error) {
	off := map[string]bool{}
	for _, d := range cfg.Disable {
		off[d] = true
	}
	r := &Redactor{onMatch: onMatch, extra: map[string]bool{}}
	for _, tr := range defaultTextRules {
		if off[tr.name] || (tr.name == "kv-secret-dq" || tr.name == "kv-secret-sq") && off["kv-secret"] {
			continue
		}
		tr.need = prefilter(tr.name)
		r.text = append(r.text, tr)
	}
	if !off["email"] {
		em := emailRule
		em.need = prefilter("email")
		r.text = append(r.text, em)
	}
	for i, rs := range cfg.Rules {
		re, err := regexp.Compile(rs.Pattern)
		if err != nil {
			return nil, fmt.Errorf("redact rule %d %q: %w", i, rs.Pattern, err)
		}
		rep := rs.Replace
		if rep == "" {
			rep = Redacted
		}
		r.text = append(r.text, textRule{fmt.Sprintf("custom-%d", i), re, rep, needNone})
	}
	r.keyRe = regexp.MustCompile(`(?i)^(?:[\w.-]*[._-])?(?:` + wordsAnywhere + `)(?:s|[._-][\w.-]*)?$|^[\w.-]*(?:` + wordsAtEnd + `)s?$`)
	for _, k := range cfg.Keys {
		r.extra[strings.ToLower(snake(k))] = true
	}
	return r, nil
}

// String redacts a string and reports whether it changed.
func (r *Redactor) String(s string) (string, bool) {
	changed := false
	// The prefilters read a lowercased copy, built in a stack buffer for a line up to
	// 512 bytes (the allocation is only for a longer one).
	var stack [512]byte
	low := lowerInto(stack[:0], s)
	for _, tr := range r.text {
		if tr.need != needNone && !tr.need.ok(low) {
			continue
		}
		if out := tr.re.ReplaceAllString(s, tr.replace); out != s {
			s, changed = out, true
			if r.onMatch != nil {
				r.onMatch(tr.name)
			}
			// `low` is not recomputed: a replacement only removes text or inserts
			// "[REDACTED]" (a custom replacement can only matter to custom rules, which
			// always run), and "[REDACTED]" contains no character or word any filter looks
			// for, so the stale copy can only say yes where a fresh one would say no.
		}
	}
	return s, changed
}

// lowerInto appends s to dst lowercased, the way the rules' case-insensitive
// matching sees it. ASCII letters are lowercased; the only two non-ASCII runes
// that fold to an ASCII letter under Go's (?i), the Kelvin sign (U+212A, folds
// to k) and the long s (U+017F, folds to s), are written as that letter; every
// other byte is copied. The filters look only for ASCII literals, and no other
// rune can stand in for one, so this is a faithful stand-in. A rule that folds
// any other rune to ASCII would need a case here, and the property test that
// feeds the filtered redactor every fold orbit member would catch the omission.
func lowerInto(dst []byte, s string) []byte {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			dst = append(dst, c+'a'-'A')
		case c == 0xE2 && i+2 < len(s) && s[i+1] == 0x84 && s[i+2] == 0xAA: // U+212A
			dst = append(dst, 'k')
			i += 2
		case c == 0xC5 && i+1 < len(s) && s[i+1] == 0xBF: // U+017F
			dst = append(dst, 's')
			i++
		default:
			dst = append(dst, c)
		}
	}
	return dst
}

// SensitiveKey reports whether an attribute named key holds a secret by name.
func (r *Redactor) SensitiveKey(key string) bool {
	k := snake(key)
	return r.extra[strings.ToLower(k)] || r.keyRe.MatchString(k)
}

// Attrs redacts attrs in place: the value of a sensitive key is replaced
// whole (whatever its type, however deeply it nests), and every other string
// goes through the text rules. It returns true if anything changed.
func (r *Redactor) Attrs(attrs map[string]any) bool {
	changed := false
	for k, v := range attrs {
		if r.SensitiveKey(k) {
			if s, ok := v.(string); !ok || s != Redacted {
				attrs[k] = Redacted
				changed = true
				if r.onMatch != nil {
					r.onMatch("key")
				}
			}
			continue
		}
		nv, c := r.value(v)
		if c {
			attrs[k] = nv
			changed = true
		}
	}
	return changed
}

func (r *Redactor) value(v any) (any, bool) {
	switch v := v.(type) {
	case string:
		return r.String(v)
	case map[string]any:
		return v, r.Attrs(v)
	case []any:
		changed := false
		for i, e := range v {
			if ne, c := r.value(e); c {
				v[i], changed = ne, true
			}
		}
		return v, changed
	}
	return v, false
}

// snake reads camelCase as snake_case and lowercases (accessToken ->
// access_token), so one key rule covers an app's naming style. Acronyms stay
// together: APIKey is api_key.
func snake(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				prev := s[i-1]
				nextLower := i+1 < len(s) && s[i+1] >= 'a' && s[i+1] <= 'z'
				if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') || (prev >= 'A' && prev <= 'Z' && nextLower) {
					b.WriteByte('_')
				}
			}
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

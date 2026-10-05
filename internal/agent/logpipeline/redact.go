package logpipeline

import (
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
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`), Redacted},
	{"authorization", regexp.MustCompile(`(?i)\b((?:proxy-)?authorization)(["']?\s*[:=]\s*["']?)(?:(?:bearer|basic|token|digest)\s+)?[^\s"',;]+`), "${1}${2}" + Redacted},
	{"url-secret", regexp.MustCompile(`(?i)([?&](?:token|code|key|signature|sig|api_?key|access_token|id_token|refresh_token|password|secret|reset_token|verify_token)=)[^&\s"'#]+`), "${1}" + Redacted},
	// key=value, key: value, "key": "value", key=>"value" (Ruby's inspect). The value is
	// a quoted string (spaces allowed) or a bare run; a quoted one keeps its quotes.
	{"kv-secret-dq", kvRule(`"[^"]*"`), `${1}${2}"` + Redacted + `"`},
	{"kv-secret-sq", kvRule(`'[^']*'`), `${1}${2}'` + Redacted + `'`},
	// A bare value may not start with '>' (that is the tail of Ruby's =>) and an
	// already-redacted value is matched whole, so applying the rules twice
	// changes nothing: "[REDACTED]" would otherwise match as "[REDACTED" and
	// leave a stray bracket behind.
	{"kv-secret", kvRule(`\[REDACTED\]|[^\s"',;&})\]>][^\s"',;&})\]]*`), "${1}${2}" + Redacted},
}

var emailRule = textRule{"email", regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}\b`), Redacted}

func kvRule(value string) *regexp.Regexp {
	key := `\b((?:[\w.-]*[._-])?(?:` + wordsAnywhere + `)(?:s|[._-][\w.-]*)?|[\w.-]*(?:` + wordsAtEnd + `)s?)`
	return regexp.MustCompile(`(?i)` + key + `(["']?\s*(?:=>|[:=])\s*)(?:` + value + `)`)
}

type textRule struct {
	name    string
	re      *regexp.Regexp
	replace string
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
		r.text = append(r.text, tr)
	}
	if !off["email"] {
		r.text = append(r.text, emailRule)
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
		r.text = append(r.text, textRule{fmt.Sprintf("custom-%d", i), re, rep})
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
	for _, tr := range r.text {
		if out := tr.re.ReplaceAllString(s, tr.replace); out != s {
			s, changed = out, true
			if r.onMatch != nil {
				r.onMatch(tr.name)
			}
		}
	}
	return s, changed
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

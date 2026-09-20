package wire

import (
	"slices"
	"strings"
	"unicode/utf8"
)

// Limits from docs/wire-protocol.md §0 "Names and tags".
const (
	// MaxMetricNameLen bounds a metric name, in bytes.
	MaxMetricNameLen = 200
	// MaxTagKeyLen bounds the key part of a tag, in bytes.
	MaxTagKeyLen = 100
	// MaxTagLen bounds a whole tag ("key:value"), in bytes.
	MaxTagLen = 200
	// MaxTagsPerPoint bounds the tags on one series. Tags multiply series
	// count (every distinct combination is a new series), so the cap exists
	// to protect storage, not the wire.
	MaxTagsPerPoint = 50
)

// ValidMetricName reports whether name matches ^[a-zA-Z][a-zA-Z0-9_.]{0,199}$.
func ValidMetricName(name string) bool {
	if name == "" || len(name) > MaxMetricNameLen || !isLetter(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !isNameChar(name[i]) {
			return false
		}
	}
	return true
}

// NormalizeMetricName replaces every character a metric name may not contain
// with '_' and reports whether the result is valid. It fails only for names
// that no replacement can fix: empty, too long, or not starting with a
// letter. Replacing rather than rejecting is deliberate — a stray '-' or
// space in an app's metric name should cost it nothing but a cosmetic '_'.
func NormalizeMetricName(name string) (string, bool) {
	if ValidMetricName(name) {
		return name, true // the common case allocates nothing
	}
	if name == "" || len(name) > MaxMetricNameLen || !isLetter(name[0]) {
		return "", false
	}
	b := []byte(name)
	for i := 1; i < len(b); i++ {
		if !isNameChar(b[i]) {
			b[i] = '_'
		}
	}
	return string(b), true
}

// ValidTag reports whether tag is a well-formed "key:value" or bare "key":
// key ^[a-z][a-z0-9_.\-/]{0,99}$, whole tag at most 200 bytes of UTF-8, and
// no ',' (the statsd tag separator) or newline anywhere. The value is free
// text otherwise, including ':' (url:http://x) and upper case — only the
// agent lowercases; the intake accepts whatever a well-formed sender chose.
func ValidTag(tag string) bool {
	if tag == "" || len(tag) > MaxTagLen || !utf8.ValidString(tag) || strings.ContainsAny(tag, ",\n") {
		return false
	}
	key, _ := SplitTag(tag)
	return validTagKey(key)
}

// NormalizeTag lowercases tag, replaces characters a key may not contain with
// '_', drops a trailing ':' ("k:" is the bare tag "k"), and reports whether
// the result is valid. Tags that can't be repaired — a key not starting with
// a letter, anything too long, invalid UTF-8 — are rejected rather than
// truncated: a truncated tag silently merges distinct series.
func NormalizeTag(tag string) (string, bool) {
	tag = strings.TrimSuffix(tag, ":")
	if !utf8.ValidString(tag) {
		return "", false
	}
	if needsLower(tag) {
		tag = strings.ToLower(tag)
	}
	key, value := SplitTag(tag)
	if key == "" || len(key) > MaxTagKeyLen || !isLower(key[0]) {
		return "", false
	}
	if !validTagKey(key) {
		k := []byte(key)
		for i := 1; i < len(k); i++ {
			if !isTagKeyChar(k[i]) {
				k[i] = '_'
			}
		}
		tag = JoinTag(string(k), value)
	}
	if !ValidTag(tag) {
		return "", false
	}
	return tag, true
}

// SplitTag splits "key:value" at the first ':'. A bare tag has an empty value.
func SplitTag(tag string) (key, value string) {
	if i := strings.IndexByte(tag, ':'); i >= 0 {
		return tag[:i], tag[i+1:]
	}
	return tag, ""
}

// JoinTag is SplitTag's inverse: "key:value", or "key" when value is empty.
func JoinTag(key, value string) string {
	if value == "" {
		return key
	}
	return key + ":" + value
}

// CanonicalTags sorts tags and removes duplicates in place, returning the
// shortened slice. Tags are a set; this is the one representation of that
// set, so the same tags in any order name the same series.
func CanonicalTags(tags []string) []string {
	slices.Sort(tags)
	return slices.Compact(tags)
}

// HasTagKey reports whether any tag in tags has the given key.
func HasTagKey(tags []string, key string) bool {
	for _, t := range tags {
		if k, _ := SplitTag(t); k == key {
			return true
		}
	}
	return false
}

func validTagKey(key string) bool {
	if key == "" || len(key) > MaxTagKeyLen || !isLower(key[0]) {
		return false
	}
	for i := 1; i < len(key); i++ {
		if !isTagKeyChar(key[i]) {
			return false
		}
	}
	return true
}

func needsLower(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; ('A' <= c && c <= 'Z') || c >= utf8.RuneSelf {
			return true
		}
	}
	return false
}

func isLower(c byte) bool  { return 'a' <= c && c <= 'z' }
func isLetter(c byte) bool { return isLower(c) || ('A' <= c && c <= 'Z') }
func isDigit(c byte) bool  { return '0' <= c && c <= '9' }

func isNameChar(c byte) bool { return isLetter(c) || isDigit(c) || c == '_' || c == '.' }

func isTagKeyChar(c byte) bool {
	return isLower(c) || isDigit(c) || c == '_' || c == '.' || c == '-' || c == '/'
}

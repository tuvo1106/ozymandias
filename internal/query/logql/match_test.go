package logql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tuvo1106/ozymandias/pkg/wire"
)

// logFrom builds a log from JSON attrs, as the wire decoder would (numbers
// as json.Number).
func logFrom(t testing.TB, msg, status string, attrs string, tags ...string) *wire.Log {
	t.Helper()
	l := &wire.Log{Ts: 1, Message: msg, Status: status, Service: "api", Source: "s", Host: "h1", Tags: tags}
	if attrs != "" {
		dec := json.NewDecoder(strings.NewReader(attrs))
		dec.UseNumber()
		if err := dec.Decode(&l.Attrs); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func matches(t testing.TB, q string, l *wire.Log) bool {
	t.Helper()
	n, err := Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	return Compile(n)(l)
}

func TestMatch_Text(t *testing.T) {
	l := logFrom(t, "Connection REFUSED by upstream", "error", `{"user":{"name":"Zoë"},"tags":["Alpha","beta"],"n":5}`)
	for q, want := range map[string]bool{
		"connection":        true,
		"REFUSED":           true,
		"refused by":        false, // two terms: "refused" AND "by" is true; see below
		`"refused by"`:      true,
		`"refused  by"`:     false,
		"conn*upstream":     true,
		"upstream*conn":     false, // order matters
		"*":                 true,
		"nomatch":           false,
		"zoë":               true, // an attribute string, folded across non-ASCII
		"ZOË":               true,
		"alpha":             true,  // an element of an array attribute
		`"5"`:               false, // numbers are not free text
		"":                  true,
		"-connection":       false,
		"-nomatch":          true,
		"connection OR zzz": true,
		"zzz OR yyy":        false,
	} {
		if q == "refused by" {
			want = true // juxtaposition is AND of two words
		}
		if got := matches(t, q, l); got != want {
			t.Errorf("%q = %v, want %v", q, got, want)
		}
	}
}

func TestMatch_Labels(t *testing.T) {
	l := logFrom(t, "m", "error", "", "env:dev", "version:1")
	l.TraceID = "abc123"
	for q, want := range map[string]bool{
		"service:api": true, "service:ap*": true, "service:*pi": true, "service:a*i": true,
		"service:API":  false, // labels are case-sensitive
		"service:ap":   false, // whole-field, not substring
		"status:error": true, "status:warn": false,
		"source:s": true, "host:h1": true, "host:h*": true,
		"env:dev": true, "env:prod": false, "env:*": true,
		"trace_id:abc123": true, "trace_id:abc": false,
		"-service:api": false, "-service:other": true,
		`service:"api"`: true,
	} {
		if got := matches(t, q, l); got != want {
			t.Errorf("%q = %v, want %v", q, got, want)
		}
	}
	bare := &wire.Log{Ts: 1, Status: "info", Service: "api"}
	for q, want := range map[string]bool{
		"env:*": false, "-env:*": true, "host:*": false, "-host:*": true,
		"env:dev": false, "-env:dev": true, "source:*": false,
	} {
		if got := matches(t, q, bare); got != want {
			t.Errorf("missing label: %q = %v, want %v", q, got, want)
		}
	}
}

func TestMatch_Attrs(t *testing.T) {
	l := logFrom(t, "m", "info", `{
		"ms": 250, "ratio": 0.5, "big": 9007199254740993, "ok": true, "none": null, "path": "/api/comics",
		"http": {"status": 404, "method": "GET"}, "http.flat": 7,
		"items": [{"sku": "A1", "qty": 2}, {"sku": "B2", "qty": 9}], "codes": [200, 500], "nested": [[1, 2], [3]],
		"s": "250"}`)
	for q, want := range map[string]bool{
		"@ms:250": true, "@ms:250.0": true, "@ms:25": false, "@ms:*": true,
		"@ms:>200": true, "@ms:>250": false, "@ms:>=250": true, "@ms:<250": false, "@ms:<=250": true,
		"@ms:[200 TO 300]": true, "@ms:[250 TO 250]": true, "@ms:[251 TO 300]": false,
		"@ratio:>0.4": true, "@ratio:0.5": true,
		"@big:9007199254740993": true, // exactness: no float64 rounding in the way the text prints
		"@ok:true":              true, "@ok:false": false,
		"@none:null":   true,
		"@path:/api/*": true, "@path:/api/comics": true, "@path:/api": false,
		"@http.status:404": true, "@http.status:>=400": true, "@http.method:GET": true,
		"@http.flat:7":  true, // a key that itself contains a dot
		"@items.sku:B2": true, "@items.qty:>5": true, "@items.sku:C3": false,
		"@nested:3": true, "@nested:>=2": true, "@nested:4": false,
		"@codes:500": true, "@codes:>=500": true, "@codes:404": false,
		"@missing:*": false, "@missing:x": false, "-@missing:x": true, "@missing:>1": false,
		"@s:>100":           false, // a numeric string is not a number for comparisons
		"@s:250":            true,  // but it equals its own text
		"@ms:>100 @ok:true": true, "@ms:>300 OR @ok:true": true,
	} {
		if got := matches(t, q, l); got != want {
			t.Errorf("%q = %v, want %v", q, got, want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	for _, c := range []struct {
		pat, s string
		want   bool
	}{
		{"abc", "abc", true}, {"abc", "abd", false}, {"a*", "a", true}, {"a*", "", false},
		{"*a", "ba", true}, {"*a", "ab", false}, {"a*c", "ac", true}, {"a*c", "abc", true},
		{"a*c", "abcd", false}, {"a**c", "abc", true}, {"*", "", true}, {"**", "x", true},
		{"a*b*c", "aXbYc", true}, {"a*b*c", "acb", false}, {"a*a", "a", false}, {"a*a", "aa", true},
		{"ab*ab", "ab", false}, {"ab*ab", "abab", true},
	} {
		if got := globMatch(c.pat, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pat, c.s, got, c.want)
		}
	}
}

func TestContainsWild(t *testing.T) {
	for _, c := range []struct {
		s    string
		pat  string
		want bool
	}{
		{"Hello World", "hello", true}, {"Hello World", "o w", true}, {"Hello World", "world!", false},
		{"Hello World", "h*d", true}, {"Hello World", "d*h", false}, {"abab", "ab*ab", true}, {"aba", "ab*ab", false},
		{"", "x", false}, {"", "", true}, {"Grüße", "grüße", true}, {"GRÜSSE", "grüsse", true},
	} {
		if got := containsWild(c.s, splitWild(strings.ToLower(c.pat))); got != c.want {
			t.Errorf("containsWild(%q, %q) = %v, want %v", c.s, c.pat, got, c.want)
		}
	}
}

// Free-text search runs over every line of a scan, so on ASCII it must not
// allocate: the allocation is what would make a 2 GiB scan slow.
func TestContainsWild_ASCIIDoesNotAllocate(t *testing.T) {
	segs := splitWild("refused")
	s := strings.Repeat("some ordinary log line CONNECTION REFUSED by upstream ", 4)
	if n := testing.AllocsPerRun(100, func() { containsWild(s, segs) }); n != 0 {
		t.Errorf("%v allocations per search", n)
	}
}

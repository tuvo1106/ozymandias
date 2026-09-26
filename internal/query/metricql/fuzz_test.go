package metricql

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// FuzzParse holds the parser to three rules on arbitrary bytes:
//
//   - it never panics. The parser runs on whatever arrives at the query API,
//     before anything has decided the request is reasonable.
//   - it never returns an error without a usable column. A parse error is a
//     thing the editor draws, so "col 0" or a column past the end of the text
//     would be a UI bug reported as a parser bug.
//   - whatever it accepts, it accepts again after printing, as the same tree.
//     This is the round-trip property from property_test.go checked against
//     inputs nobody designed — the generator there can only produce trees the
//     author thought of, and the corpus here is shaped by coverage instead.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"",
		"avg:x{*}",
		"sum:http.request.count{service:api,!status:5*} by {route}.as_rate()",
		"p95:d{a IN (b,c),$env} by {k}.rollup(max, 60).fill(zero)",
		`top(sum:x{*} by {k}, 5, "mean", "desc") / -2 + abs(1e3)`,
		"clamp_min(histogram_quantile(0.9, sum:h{*} by {upper_bound}), 0)",
		"((((1))))",
		"a{",
		"sum:x{k:}",
		"1 - -2 - (3 - 4)",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		n, err := Parse(src)
		if err != nil {
			var perr *Error
			if !errors.As(err, &perr) {
				t.Fatalf("Parse(%q) returned %T, want *Error", src, err)
			}
			if perr.Col < 1 || perr.Col > len(src)+1 {
				t.Fatalf("Parse(%q): col %d is outside the text", src, perr.Col)
			}
			if strings.TrimSpace(perr.Msg) == "" {
				t.Fatalf("Parse(%q): an error with no message", src)
			}
			return
		}
		text := n.String()
		again, err := Parse(text)
		if err != nil {
			t.Fatalf("Parse(%q) printed %q, which does not parse: %v", src, text, err)
		}
		if !reflect.DeepEqual(again, n) {
			t.Fatalf("Parse(%q) printed %q, which parses differently:\n %#v\n %#v", src, text, n, again)
		}
		if again.String() != text {
			t.Fatalf("Parse(%q): printing is not stable: %q then %q", src, text, again.String())
		}
	})
}
